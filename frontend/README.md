# nasctl Svelte frontend

The console uses Svelte 5 and Vite. Editable source lives in `src`; `npm run build`
writes the three static assets embedded by Go in `internal/hubweb/web` and adds
content hashes to their URLs. Generated assets are committed so Go-only and
cross-architecture builds do not require Node. Node never runs on the NAS.

Use Node 22.12+ or 24 LTS:

```sh
npm ci
npm run check
npm run build
```

For local development, run `nasctl serve` on 127.0.0.1:18090, then `npm run dev`.
Vite proxies `/api` to that local server. All production requests use the Go
server's same-origin API. Authentication uses the existing cookie; frontend code
never writes credentials to localStorage. Theme, file-view preference and explicitly
pinned root-relative folder locations are stored locally. Recent files and location
tabs live only in the mounted file app; no file contents are stored in the browser.

The desktop and Finder-style redesign plan was reviewed with Antigravity Claude
Opus 5.5 (High), then implemented here. See [the plan](../docs/FRONTEND-REDESIGN.md).
Details and validation
limits are in [docs/FRONTEND.md](../docs/FRONTEND.md). The DOM harness in
`scripts/uitest` exercises the compiled bundle against a fixture Go server.
