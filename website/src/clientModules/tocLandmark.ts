// Governing: ADR-0014, SPEC-0010 REQ "WCAG 2.1 AA & Semantics"
//
// The on-this-page column is the one required landmark theme-classic does not
// render. The other two were verified rather than assumed:
// `DocSidebar/Desktop/Content` renders `<nav aria-label="Docs sidebar">` and
// `DocRoot/Layout/Main` renders `<main>`. `TOC` renders a plain `<div>` around
// `<ul class="table-of-contents">`, so the list structure is exposed but the
// region is not, and a screen-reader user cannot reach it by landmark.
//
// WHY A CLIENT MODULE AND NOT A WRAP. The obvious fix is to wrap
// `@theme/DocItem/TOC/Desktop` in a `<nav>`. SPEC-0010 REQ "Site Chrome and
// Layout" allows exactly ONE wrapped theme component and requires that wrap to
// be justified in design.md; design.md spends its single budgeted wrap nowhere
// and keeps `DocSidebar` named as the reserved candidate. Spending the whole
// budget here — from an accessibility story, for two ARIA attributes, on a
// component `theme-classic` marks neither wrap-safe nor eject-safe — would burn
// the project's one override on its cheapest possible use.
//
// `clientModules` is a first-class Docusaurus configuration key, not a theme
// override: it costs nothing from that budget and survives an upgrade for as
// long as `ThemeClassNames.docs.docTocDesktop` keeps its value.
//
// The cost, stated plainly: the attributes are applied after hydration, so they
// are absent from the served HTML. Assistive technology reads the live
// accessibility tree of a hydrated page, so this is correct in use; it is not
// correct for a reader with JavaScript disabled, who still gets the list and
// its headings and loses only the landmark shortcut.

import ExecutionEnvironment from '@docusaurus/ExecutionEnvironment';

/** `ThemeClassNames.docs.docTocDesktop` — the stable class `TOC` is handed. */
const TOC_DESKTOP_CLASS = 'theme-doc-toc-desktop';

/** Phrased like theme-classic's own "Docs sidebar" landmark. */
const TOC_LABEL = 'On this page';

function markTableOfContents(): void {
  const nodes = document.querySelectorAll<HTMLElement>(`.${TOC_DESKTOP_CLASS}`);
  for (const node of nodes) {
    // Idempotent: the observer below fires on every DOM mutation, so the common
    // case has to be a cheap no-op rather than a repeated write.
    if (node.getAttribute('role') === 'navigation') {
      continue;
    }
    node.setAttribute('role', 'navigation');
    node.setAttribute('aria-label', TOC_LABEL);
  }
}

/*
 * A MutationObserver rather than the `onRouteDidUpdate` lifecycle, and the
 * difference is a real timing bug rather than taste. The desktop TOC is mounted
 * conditionally on `useWindowSize()`, so crossing the 996px breakpoint destroys
 * and recreates the node with no route change at all — and a `resize` listener
 * registered here fires BEFORE the one `useWindowSize` registers from an
 * effect, so it would run against the DOM as it was before React re-rendered.
 * Observing mutations removes the ordering question: the node is marked when it
 * appears, whatever put it there.
 *
 * The callback is one `querySelectorAll` over a class that matches at most one
 * element, and `attributes` is deliberately not observed — the TOC's active
 * link changes class on scroll, and watching that would spin the callback for
 * every pixel.
 */
if (ExecutionEnvironment.canUseDOM) {
  markTableOfContents();
  new MutationObserver(markTableOfContents).observe(document.documentElement, {
    childList: true,
    subtree: true,
  });
}
