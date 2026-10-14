# Frozen operator shell (`/app/`)

This package embeds a minimal PWA for local development only.

**Canonical UI:** pg-react (separate repository). Do not add features here.

**When the server mounts `/app/*`:** `APP_ENV` is not `production`, or `FRONTEND_URL` is empty.

**Embedded SPA:** Production builds use `internal/web/dist/` on the HTTP catch-all route.
