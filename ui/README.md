# agl-gateway portal UI

The management & inspection SPA for `agl-gateway`, built with **Vite + React + TypeScript +
Tailwind v4 + shadcn/ui**. It talks to the master-key control plane (`/admin/*`) and is served
by the gateway under `/portal`.

## Develop

```sh
npm ci
npm run dev      # Vite dev server with HMR
```

The dev server needs the gateway's API. Either run the gateway and use a proxy, or open the
built portal from the gateway directly (see below). API calls use absolute paths
(`/admin/...`, `/healthz`), so point a dev proxy at a running gateway if you use `npm run dev`.

## Build

```sh
npm run build
```

This type-checks, bundles, and emits the production build into **`../internal/portal/dist`**,
which the Go binary embeds via `go:embed`. The output is gitignored; a `postbuild` step
re-creates the `.gitkeep` anchor that keeps the embed target present on a clean checkout. CI
and the Docker image run this build automatically before the Go build.

## Structure

- `src/lib/` — API client (`api.ts`), types, formatters, time-range + auth helpers.
- `src/components/ui/` — shadcn/ui primitives.
- `src/components/` — app components (`Login`, `LogDrawer`, `TimeRangePicker`, …).
- `src/components/tabs/` — the Keys, Logs, Usage, and Test-models tabs.
