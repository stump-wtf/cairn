// Governing: ADR-0014, SPEC-0010 REQ "WCAG 2.1 AA & Semantics"
//            SPEC-0010 REQ "Dynamic Content Regions"
//
// Two gaps in theme-classic's on-this-page column, neither reachable from CSS.
//
// LANDMARK. `DocSidebar/Desktop/Content` renders `<nav aria-label="Docs
// sidebar">` and `DocRoot/Layout/Main` renders `<main>` — verified, not
// assumed. `TOC` renders a plain `<div>` around `<ul class="table-of-contents">`
// and `TOCCollapsible` likewise, so on the desktop and the mobile layout alike
// the list structure is exposed and the region is not: a screen-reader user
// cannot reach the table of contents by landmark. REQ "WCAG 2.1 AA & Semantics"
// asks for a landmark on "the navigation rail, the main content, and the table
// of contents", and the third is the one the theme does not ship.
//
// DISCLOSURE STATE. Below 997px `useWindowSize()` swaps the desktop column for
// `TOCCollapsible`, whose `CollapseButton` renders
// `<button type="button" class="clean-btn …">On this page</button>` and sets no
// `aria-expanded` at all — it takes `collapsed` as a prop and spends it only on
// a CSS class for the caret. So every documentation page on a phone carries a
// show/hide control that announces as a plain button and never reports whether
// the panel it controls is open. That is REQ "Dynamic Content Regions",
// scenario "Disclosure state exposed", failing across the whole mobile site.
// The theme gets the equivalent right elsewhere — the sidebar category carets
// and the navbar toggle both ship `aria-expanded` — which is what makes this an
// omission rather than a policy.
//
// WHY A CLIENT MODULE AND NOT A WRAP. The obvious fix for either is to wrap
// `@theme/DocItem/TOC/Desktop` and `@theme/TOCCollapsible`. SPEC-0010 REQ "Site
// Chrome and Layout" allows exactly ONE wrapped theme component and requires
// that wrap to be justified in design.md; design.md spends its single budgeted
// wrap nowhere and keeps `DocSidebar` named as the reserved candidate. Spending
// the whole budget here — from an accessibility story, for three ARIA
// attributes, on components `theme-classic` marks neither wrap-safe nor
// eject-safe — would burn the project's one override on its cheapest possible
// use, and it would cost two wraps rather than one.
//
// `clientModules` is a first-class Docusaurus configuration key, not a theme
// override: it costs nothing from that budget and survives an upgrade for as
// long as the class names below keep their values.
//
// BOTH COLUMNS ARE MARKED, AND ONLY ONE IS EVER EXPOSED. `DocItem/Layout`
// mounts the desktop column only at `windowSize === 'desktop' | 'ssr'` but
// mounts the mobile one unconditionally, hiding it above 997px with
// `display: none` to avoid a hydration FOUC. A `display: none` subtree is out of
// the accessibility tree, so a reader is never offered two "On this page"
// landmarks even though both nodes exist in the DOM below the fold.
//
// The cost, stated plainly: the attributes are applied after hydration, so they
// are absent from the served HTML. Assistive technology reads the live
// accessibility tree of a hydrated page, so this is correct in use; it is not
// correct for a reader with JavaScript disabled, who keeps the list and its
// headings and loses the landmark shortcut — and who cannot open the
// collapsible panel at all, so on the disclosure half there is no state left to
// misreport.

import ExecutionEnvironment from '@docusaurus/ExecutionEnvironment';

/** `ThemeClassNames.docs.docTocDesktop` — the stable class `TOC` is handed. */
const TOC_DESKTOP_CLASS = 'theme-doc-toc-desktop';

/** `ThemeClassNames.docs.docTocMobile` — the same, for `TOCCollapsible`. */
const TOC_MOBILE_CLASS = 'theme-doc-toc-mobile';

/**
 * The CSS-module local name theme-classic puts on the collapsible wrapper while
 * it is open (`TOCCollapsible/styles.module.css`), hashed at build time into
 * `tocCollapsibleExpanded_XXXX`. Matched as a prefix because the hash is not
 * ours to predict.
 *
 * Reading the theme's own class is deliberate. It is the single flag the theme
 * derives from `collapsed`, and it is the flag that rotates the caret — so if a
 * future release renames it, the visible affordance breaks in the same release
 * and in the same place, instead of the ARIA quietly desynchronising while the
 * page still looks right. The alternative, inferring state from the panel's
 * inline `display`, lags a collapse by the length of the height transition.
 */
const EXPANDED_CLASS_PREFIX = 'tocCollapsibleExpanded';

/** The panel the button controls, for `aria-controls`. */
const PANEL_CLASS_PREFIX = 'tocCollapsibleContent';

/** Phrased like theme-classic's own "Docs sidebar" landmark. */
const TOC_LABEL = 'On this page';

const PANEL_ID = 'cairn-toc-panel';

const hasPrefixedClass = (node: Element, prefix: string): boolean =>
  Array.from(node.classList).some((name) => name.startsWith(prefix));

function markLandmark(node: HTMLElement): void {
  // Idempotent: the observer below fires on every DOM mutation, so the common
  // case has to be a cheap no-op rather than a repeated write.
  if (node.getAttribute('role') === 'navigation') return;
  node.setAttribute('role', 'navigation');
  node.setAttribute('aria-label', TOC_LABEL);
}

/**
 * Publish the collapsible's open/closed state on its own button.
 *
 * `Collapsible` is rendered with `lazy`, so the panel does not exist in the DOM
 * until the first expansion. `aria-controls` is therefore set when the panel
 * appears rather than up front: `aria-controls` pointing at an id that is not in
 * the document is worse than no `aria-controls`.
 */
function syncDisclosure(container: HTMLElement): void {
  const button = container.querySelector('button');
  if (!button) return;

  button.setAttribute(
    'aria-expanded',
    String(hasPrefixedClass(container, EXPANDED_CLASS_PREFIX)),
  );

  const panel = Array.from(container.children).find(
    (child) => child !== button && hasPrefixedClass(child, PANEL_CLASS_PREFIX),
  );
  if (panel) {
    if (!panel.id) panel.id = PANEL_ID;
    button.setAttribute('aria-controls', panel.id);
  }
}

/*
 * Toggling the panel rewrites the wrapper's className and changes nothing else
 * the tree observer below watches, so the disclosure needs an attribute watch of
 * its own. It is scoped to the wrapper element and to `class`, and emphatically
 * not `subtree`: the on-this-page links inside re-class themselves as the reader
 * scrolls, and watching those would spin the callback for every pixel.
 *
 * Setting `aria-expanded` from here targets the BUTTON, which is not observed,
 * so the callback cannot retrigger itself.
 */
const classObserver = ExecutionEnvironment.canUseDOM
  ? new MutationObserver((records) => {
      for (const record of records) syncDisclosure(record.target as HTMLElement);
    })
  : undefined;

function markTableOfContents(): void {
  for (const node of document.querySelectorAll<HTMLElement>(`.${TOC_DESKTOP_CLASS}`)) {
    markLandmark(node);
  }
  for (const node of document.querySelectorAll<HTMLElement>(`.${TOC_MOBILE_CLASS}`)) {
    markLandmark(node);
    syncDisclosure(node);
    // Re-observing an already-observed node with the same options replaces the
    // registration rather than stacking a second one, so this is free to repeat.
    classObserver?.observe(node, {attributes: true, attributeFilter: ['class']});
  }
}

/*
 * A MutationObserver rather than the `onRouteDidUpdate` lifecycle, and the
 * difference is a real timing bug rather than taste. Both columns are mounted
 * conditionally on `useWindowSize()`, so crossing the 996px breakpoint destroys
 * one and creates the other with no route change at all — and a `resize`
 * listener registered here fires BEFORE the one `useWindowSize` registers from
 * an effect, so it would run against the DOM as it was before React
 * re-rendered. Observing mutations removes the ordering question: the node is
 * marked when it appears, whatever put it there. It is also what picks up the
 * lazily mounted panel on the first expansion.
 *
 * The callback is two `querySelectorAll` calls over classes that match at most
 * one element each, and `attributes` is deliberately not observed here for the
 * scroll-churn reason given above.
 */
if (ExecutionEnvironment.canUseDOM) {
  markTableOfContents();
  new MutationObserver(markTableOfContents).observe(document.documentElement, {
    childList: true,
    subtree: true,
  });
}
