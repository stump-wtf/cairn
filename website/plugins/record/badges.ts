// Governing: ADR-0014, SPEC-0010 REQ "Derived Design-Language Page"
//
// The type-badge set, read out of the record.
//
// SPEC-0010 names "the type-badge set" as one of the four things the
// design-language page renders, alongside the surface ramp, the category
// accents, and the two type families. It is a distinct thing from the category
// accents: a badge is a share type's short code — `MD`, `TRJ` — while a category
// is an ADR-0009 span category. Rendering the accent set twice and calling the
// second copy "type badges" published a set the record does not contain.
//
// The record does contain the real one, in prose rather than in a table, so the
// extraction is stated as a rule narrow enough to be checkable:
//
//   a badge code is an INLINE CODE SPAN of two to five upper-case characters,
//   inside a block that uses the word "badge".
//
// That is not a heuristic over prose the way "a heading containing the word
// endpoint" would be. It matches the record's own single convention for writing
// one — ADR-0002's `**Badge** — … (`MD`, `PY`/lang, `IMG`, `FILE`/`GZ`, `HK`,
// `TRJ`; …)`, ADR-0009's "badge `TRJ`", SPEC-0005's "(badge `HK`, …)" — and it
// picks up nothing else in the record as it stands. It also fails LOUDLY rather
// than silently: a record with no badge declaration at all breaks the build
// instead of publishing an empty section.
//
// What the record does NOT contain, anywhere, is a mapping from a share type to
// an ADR-0009 span category. The page therefore may not claim one, and does not.

/* eslint-disable @typescript-eslint/no-explicit-any */

import type {BadgeEntry, ParsedRecord, RecordRef} from './types.ts';

type Node = any;

/**
 * A badge code as the record writes one: two to five upper-case characters,
 * digits allowed after the first. Wide enough for `MD` and `FILE`, narrow enough
 * that a spelled-out constant or a header name in a code span is not mistaken
 * for one.
 */
const BADGE_CODE = /^[A-Z][A-Z0-9]{1,4}$/;

/** The word that makes a block a badge declaration. */
const BADGE_WORD = /\bbadges?\b/i;

/** Blocks that scope a declaration: a badge is declared in a sentence. */
const TEXT_BLOCKS = new Set(['paragraph', 'heading', 'tableCell']);

/**
 * Every badge code declared in one parsed record, in document order.
 *
 * Scoped per text block rather than per file: "badge" somewhere in a decision
 * must not turn every upper-case code span in that decision into a badge.
 */
export async function collectBadgeCodes(tree: Node): Promise<string[]> {
  const {toString} = await import('mdast-util-to-string');
  const codes: string[] = [];

  const codeSpans = (node: Node, into: string[]): void => {
    if (node.type === 'inlineCode') {
      into.push(String(node.value ?? ''));
      return;
    }
    for (const child of (node.children ?? []) as Node[]) {
      codeSpans(child, into);
    }
  };

  const walk = (node: Node): void => {
    if (TEXT_BLOCKS.has(node.type)) {
      if (BADGE_WORD.test(toString(node))) {
        const spans: string[] = [];
        codeSpans(node, spans);
        for (const span of spans) {
          if (BADGE_CODE.test(span) && !codes.includes(span)) {
            codes.push(span);
          }
        }
      }
      return;
    }
    for (const child of (node.children ?? []) as Node[]) {
      walk(child);
    }
  };

  walk(tree);
  return codes;
}

/**
 * The published badge set: every code the record declares, with every record
 * that declares it.
 *
 * Order is first-declaration order over the records in the order they are
 * given, which is the record's own order — ADR-0002 defines the `ShareType`
 * seam and lists the set, so the page reads in the order that decision wrote.
 * Alphabetising would be a fact the record does not state.
 */
export function buildBadgeSet(
  records: readonly Pick<ParsedRecord, 'id' | 'badgeCodes'>[],
  refs: Readonly<Record<string, RecordRef>>,
): BadgeEntry[] {
  const byCode = new Map<string, BadgeEntry>();

  for (const record of records) {
    for (const code of record.badgeCodes) {
      const ref = refs[record.id];
      if (!ref) {
        continue;
      }
      const entry = byCode.get(code);
      if (entry) {
        entry.sources.push(ref);
      } else {
        byCode.set(code, {code, sources: [ref]});
      }
    }
  }

  const badges = [...byCode.values()];
  if (badges.length === 0) {
    throw new Error(
      'no share-type badge is declared anywhere in the design record. The ' +
        'design-language page renders the badge set derived from it, so an empty ' +
        'set means the extraction in website/plugins/record/badges.ts has stopped ' +
        'matching how the record writes a badge — fix the rule rather than ' +
        'publishing an empty section or a hand-typed list.',
    );
  }
  return badges;
}
