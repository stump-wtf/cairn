// Governing: ADR-0014, SPEC-0010 REQ "Derived Design-Language Page"
//
// The design language, read out of the token definition instead of retyped.
//
// SPEC-0010 requires a page whose swatches "take [their] value from the token
// definition rather than restating it", and whose scenario is explicit: change a
// token, rebuild, and the page shows the new value with NO page file edited. A
// swatch painted with `var(--cat-reason)` satisfies half of that for free — the
// browser resolves it — but a design-language page that will not tell you what
// `--cat-reason` actually IS is a colour chart with the numbers cut off. So the
// values are read out of `src/css/custom.css` at build time and emitted as data.
//
// Two deliberate choices about where this lives.
//
// It is a SEPARATE generated module from `record.json`. SPEC-0010 REQ "Derived
// Data Module" names what that module carries — the decision list, the spec
// list, the counts, the graph — and this is not derived from the record at all;
// it is derived from a stylesheet. Folding a stylesheet inventory into the
// record's module would make that requirement's own description of it false, and
// would put the palette into every chunk that reads a count.
//
// It reuses `scripts/check-tokens.mjs` rather than parsing CSS a second time.
// That script is already the build's authority on what the token file contains —
// it is what fails the build on a stray hex literal — and a second, subtly
// different CSS parser inside the same site is exactly the kind of duplicate
// truth ADR-0014 is about. `adr0009Categories()` likewise: the split between
// "category ADR-0009 recommends" and "the neutral default" is read from the
// decision record, so the page's neutral-default statement is derived rather
// than asserted.

import fs from 'node:fs/promises';
import path from 'node:path';

// A plain `.mjs` script with no type declarations; the site config already
// imports `assertTokens` from it exactly this way.
import {adr0009Categories, parseDeclarations} from '../../scripts/check-tokens.mjs';

import {repoRelative, type RecordPaths} from './paths.ts';

export interface TokenSwatch {
  /** The custom property name, e.g. `--cairn-ink-100`. */
  token: string;
  /** Its resolved value, e.g. `#131418`. */
  value: string;
}

export interface CategoryToken extends TokenSwatch {
  /** The category the token names, e.g. `reason`. */
  name: string;
  /**
   * True when ADR-0009's recommended set names this category. The one token for
   * which this is false is the neutral default every other string falls back to.
   */
  recommended: boolean;
}

export interface TokenRamp {
  /** `ink` / `paper`, taken from the token prefix rather than named here. */
  name: string;
  tokens: TokenSwatch[];
}

export interface TypeFamily {
  token: string;
  /** The declared stack, fallbacks and all. */
  stack: string;
  /** The first family in the stack — the one the design language actually asks for. */
  primary: string;
}

export interface DesignTokens {
  /** Repo-relative path of the file every value here was read from. */
  sourcePath: string;
  categories: CategoryToken[];
  ramps: TokenRamp[];
  families: TypeFamily[];
}

/** The surface ramps, by token prefix. The ramp NAME is the prefix's last word. */
const RAMP_PREFIXES = ['--cairn-ink-', '--cairn-paper-'] as const;

/** The two type roles, in the order the token definition declares them. */
const FAMILY_TOKENS = ['--cairn-font-sans', '--cairn-font-mono'] as const;

const CATEGORY_PREFIX = '--cat-';
const HEX = /^#[0-9a-fA-F]{3,8}$/;

interface Declaration {
  selector: string;
  prop: string;
  value: string;
  line: number;
}

/**
 * Resolve a `:root` custom property through its `var()` chain to a literal.
 *
 * The ramps are not all literals: `--cairn-paper-1000` is declared as
 * `var(--cairn-ink-0)`, and a swatch that printed the indirection instead of the
 * colour would be restating nothing useful. Cycles return null rather than
 * looping.
 */
function makeResolver(declarations: Declaration[]): (name: string) => string | null {
  const root = new Map<string, string>();
  for (const declaration of declarations) {
    if (
      declaration.prop.startsWith('--') &&
      /(^|,\s*):root$/.test(declaration.selector)
    ) {
      root.set(declaration.prop, declaration.value);
    }
  }
  const resolve = (name: string, seen = new Set<string>()): string | null => {
    if (seen.has(name)) {
      return null;
    }
    seen.add(name);
    const value = root.get(name);
    if (!value) {
      return null;
    }
    if (HEX.test(value)) {
      return value.toLowerCase();
    }
    const indirect = /^var\(\s*(--[\w-]+)\s*\)$/.exec(value);
    return indirect ? resolve(indirect[1]!, seen) : null;
  };
  return (name) => resolve(name);
}

/** `'IBM Plex Sans', system-ui, …` → `IBM Plex Sans`. */
function primaryFamily(stack: string): string {
  const first = stack.split(',')[0] ?? stack;
  return first.trim().replace(/^['"]|['"]$/g, '');
}

/**
 * Read the token definition and derive the design-language inventory.
 *
 * Errors here fail the build, which is the point: a design-language page that
 * silently loses the ink ramp because a prefix changed is worse than one that
 * refuses to build.
 */
export async function collectDesignTokens(
  paths: RecordPaths,
): Promise<DesignTokens> {
  const css = await fs.readFile(paths.tokenCss, 'utf8');
  const declarations = parseDeclarations(css) as Declaration[];
  const resolve = makeResolver(declarations);
  const sourcePath = repoRelative(paths, paths.tokenCss);

  const rootDeclarations = declarations.filter((declaration) =>
    /(^|,\s*):root$/.test(declaration.selector),
  );

  const recommended = new Set<string>(adr0009Categories(paths.repoRoot) as string[]);

  const categories: CategoryToken[] = [];
  for (const declaration of rootDeclarations) {
    if (!declaration.prop.startsWith(CATEGORY_PREFIX)) {
      continue;
    }
    const value = resolve(declaration.prop);
    if (!value) {
      throw new Error(
        `${sourcePath}:${declaration.line}: ${declaration.prop} does not resolve to a colour, ` +
          `so the design-language page cannot show its value.`,
      );
    }
    const name = declaration.prop.slice(CATEGORY_PREFIX.length);
    categories.push({
      token: declaration.prop,
      name,
      value,
      recommended: recommended.has(name),
    });
  }
  if (categories.length === 0) {
    throw new Error(
      `${sourcePath}: no ${CATEGORY_PREFIX}* tokens found at :root. The design-language page ` +
        `renders the ADR-0009 palette from this file and has nothing to render.`,
    );
  }
  if (!categories.some((category) => !category.recommended)) {
    throw new Error(
      `${sourcePath}: every ${CATEGORY_PREFIX}* token names a category ADR-0009 recommends, so ` +
        `there is no neutral default for an unrecognised category to fall back to.`,
    );
  }

  const ramps: TokenRamp[] = RAMP_PREFIXES.map((prefix) => {
    const tokens: TokenSwatch[] = [];
    for (const declaration of rootDeclarations) {
      if (!declaration.prop.startsWith(prefix)) {
        continue;
      }
      const value = resolve(declaration.prop);
      if (value) {
        tokens.push({token: declaration.prop, value});
      }
    }
    if (tokens.length === 0) {
      throw new Error(
        `${sourcePath}: no ${prefix}* tokens found at :root; the surface ramp the ` +
          `design-language page renders has gone missing.`,
      );
    }
    return {name: prefix.replace(/^--cairn-|-$/g, ''), tokens};
  });

  const families: TypeFamily[] = FAMILY_TOKENS.map((token) => {
    const declaration = rootDeclarations.find((item) => item.prop === token);
    if (!declaration) {
      throw new Error(`${sourcePath}: ${token} is not declared at :root.`);
    }
    return {
      token,
      stack: declaration.value,
      primary: primaryFamily(declaration.value),
    };
  });

  return {sourcePath, categories, ramps, families};
}

/**
 * Write the token inventory beside the record's derived module.
 *
 * Unchanged bytes are not rewritten, for the same reason `stage.ts` skips them:
 * this file lives under `src/`, which the dev server watches, and rewriting it
 * identically would bounce the build on every record edit.
 */
export async function stageDesignTokens(
  paths: RecordPaths,
  tokens: DesignTokens,
): Promise<string> {
  const contents = `${JSON.stringify(tokens, null, 2)}\n`;
  await fs.mkdir(path.dirname(paths.tokensJson), {recursive: true});
  let existing: string | null = null;
  try {
    existing = await fs.readFile(paths.tokensJson, 'utf8');
  } catch {
    existing = null;
  }
  if (existing !== contents) {
    await fs.writeFile(paths.tokensJson, contents, 'utf8');
  }
  return paths.tokensJson;
}
