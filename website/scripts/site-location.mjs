/**
 * Site location — the origin (`url`) and base path (`baseUrl`) a build bakes in.
 *
 * Governing: ADR-0014, SPEC-0010 REQ "Build and Deployment" — "The public base
 * URL MUST be configured in exactly one place."
 *
 * Docusaurus bakes `url` + `baseUrl` into every canonical link, og:url and
 * sitemap entry, so whatever a build falls back to is what a reader or crawler
 * is sent to. The fallback is therefore the PUBLIC face, https://cairn.stump.wtf,
 * rooted at `/`: a build nobody configured must never advertise a host the
 * public cannot reach.
 *
 * Two deployment targets build this record:
 *
 *   - the app domain (the cairn-docs image): DOCS_URL=https://cairn.stump.wtf,
 *     DOCS_BASE_URL=/ — the same as the defaults;
 *   - Gitea Pages, which serves a repo's bundle from `/<repo>/` on a host of the
 *     form `<owner>.pages.<domain>`. Its CI job exports DOCS_URL and nothing
 *     else, so when DOCS_URL names a Pages host the base path defaults to
 *     `/cairn/`. Without this the Pages bundle would be built rooted at `/`,
 *     request its assets from the host root and hydrate into the 404 page.
 *
 * Both variables are read with `||` and not `??`: CI env plumbing exports an
 * unset input as an empty string, and an empty `url` or `baseUrl` is a
 * valid-looking wrong answer.
 */

/** The public origin of the docs, and the default for every build. */
export const PUBLIC_ORIGIN = 'https://cairn.stump.wtf';

/** Where Gitea Pages serves this repo's bundle on its owner's Pages host. */
export const PAGES_BASE_URL = '/cairn/';

/**
 * True when `url` is a Gitea Pages host, `<owner>.pages.<domain>`. Matched on
 * the shape, not a hostname, so no private host name lives in this file.
 */
export function isPagesHost(url) {
  let hostname;
  try {
    hostname = new URL(url).hostname;
  } catch {
    return false;
  }
  const labels = hostname.split('.');
  return labels.length >= 4 && labels[1] === 'pages';
}

/** The `{url, baseUrl}` pair for a build, from DOCS_URL and DOCS_BASE_URL. */
export function resolveSiteLocation(env = process.env) {
  const url = env.DOCS_URL || PUBLIC_ORIGIN;
  const baseUrl = env.DOCS_BASE_URL || (isPagesHost(url) ? PAGES_BASE_URL : '/');
  return {url, baseUrl};
}
