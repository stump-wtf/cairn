// Governing: ADR-0014, SPEC-0010 REQ "Derived Design-Language Page"
//
// The one accessor for the derived token inventory. The design-language page
// reads its swatch values from here, so changing a token in
// `src/css/custom.css` changes the page and no page file is edited.
//
// `src/generated/tokens.json` is written by the pipeline and is git-ignored, so
// this import resolves only after the generator has run — the same contract
// `src/data/record.ts` has, and what `npm run generate` exists for.
//
// This is deliberately NOT part of the record's derived data module. The record
// module carries the decision list, the specification list, the counts, and the
// graph; this is derived from a stylesheet. Keeping them separate also keeps the
// palette out of every chunk that only wanted a count.
//
// The only import from `plugins/` is type-only and therefore erased: a value
// import would put generator code, and Node's `fs`, into the browser bundle.

import generated from '@site/src/generated/tokens.json';

import type {
  CategoryToken,
  DesignTokens,
  TokenRamp,
  TokenSwatch,
  TypeFamily,
} from '@site/plugins/record/tokens';

export type {CategoryToken, DesignTokens, TokenRamp, TokenSwatch, TypeFamily};

export const tokens = generated as unknown as DesignTokens;

export const categories: CategoryToken[] = tokens.categories;
export const ramps: TokenRamp[] = tokens.ramps;
export const families: TypeFamily[] = tokens.families;

/**
 * The category every string ADR-0009 does not name falls back to. Derived, not
 * asserted: `tokens.ts` reads ADR-0009's recommended set out of the decision
 * record and marks everything outside it as the neutral default.
 */
export const neutralDefault: CategoryToken | undefined = categories.find(
  (category) => !category.recommended,
);

export default tokens;
