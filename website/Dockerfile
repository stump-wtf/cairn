# Static docs image: the compiled record site served by Caddy on :80. This is
# cairn.stump.wtf's PUBLIC FACE — the marketing homepage at `/` and the record
# at `/docs/intro/` — fronted by cloud01's edge Caddy, which routes `/`,
# `/docs/*` and the bundle's static namespaces here and everything else to the
# app. Same build pattern as stump.wtf/switchboard's docs image, for the same
# reason: the site is built on the CI runner (npm ci && npm run build, with
# DOCS_URL=https://cairn.stump.wtf and DOCS_BASE_URL=/ — see the docs-image
# job in .gitea/workflows/pipeline.yaml) and ONLY the static build/ output is
# copied in — no Node stage, so this docker build fetches nothing and works on
# a BuildKit runner with no proxy egress.
#
# Build context is website/; build/ must already exist and must have been
# built with DOCS_BASE_URL=/ — a bundle based anywhere else hydrates into the
# 404 page when served at `/` (see docusaurus.config.ts).
FROM caddy:2-alpine
COPY build /srv
COPY Caddyfile /etc/caddy/Caddyfile
EXPOSE 80
