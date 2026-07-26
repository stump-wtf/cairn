# Static docs image: the compiled record site served by Caddy on :80, fronted
# at https://cairn.stump.wtf/docs/ by cloud01's edge Caddy (handle_path
# /docs/* → this container). Same pattern as stump.wtf/switchboard's
# docs image, for the same reason: the site is built on the CI runner (npm ci
# && npm run build, with DOCS_URL/DOCS_BASE_URL set — see the docs-image job
# in .gitea/workflows/pipeline.yaml) and ONLY the static build/ output is
# copied in — no Node stage, so this docker build fetches nothing and works on
# a BuildKit runner with no proxy egress.
#
# Build context is website/; build/ must already exist and must have been
# built with DOCS_BASE_URL=/docs/, or every baked href points at the Pages
# host's /cairn/ prefix instead.
FROM caddy:2-alpine
COPY build /srv
COPY Caddyfile /etc/caddy/Caddyfile
EXPOSE 80
