# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

A small, **stateful** HTTP server that feeds an **inkyframe** — a 7.3" 800×480 **7-colour ACeP** e-ink photo frame with 5 buttons (rp2040w). The frame wakes periodically, requests an image, and renders it; the server decides what to show. It runs a configurable set of **apps**, each of which keeps its own 800×480 image fresh in the background. The server **dithers** to the panel's 7-colour palette itself (not on-device) and serves a packed **4bpp framebuffer** (or an indexed PNG for browsers). A `manager` tracks the **active app** and a **rotate** flag, exposed through HTTP endpoints that map to the frame's buttons. All state is in-memory.

The only app type today is **immich**: it pulls two random images from an [Immich](https://immich.app) album (authenticated with an API key), crops each to fill a 400×480 half, stitches them into one 800×480 composite, dithers it to the 7-colour palette, and packs it into the framebuffer. See `README.md` for user-facing usage and the **firmware contract** (palette/PEN order + framebuffer packing).

## Commands

```bash
go build ./...                          # build
go run . -config config.yaml            # run the server locally (needs a config file)
go test ./...                           # run all tests
go test -race ./...                     # run tests under the race detector
go test -run TestName ./...             # run a single test
go vet ./...                            # vet
gofmt -l -w .                           # format (or: go fmt ./...)
```

## Architecture notes

The image pipeline stays **independent of HTTP, config, and Immich** so it can be
unit-tested with generated fixtures. App state and serving sit on top:

- `compose.go` — **pure, no HTTP/Immich deps.** `cropToFill` (single-step crop-to-fill via `draw.CatmullRom.Scale` of a centered, aspect-matched source sub-rectangle), `compose` (two 400×480 halves onto an 800×480 canvas), and the dither/encode layer: `inkyPalette` (the 7-colour palette; index = PEN number, the firmware contract), the `ditherAlgo` enum (`ditherFloydSteinberg`/`ditherAtkinson`, from the `dither` config key), `ditherImage` (renamed from `dither` to avoid colliding with the package; dithers to `inkyPalette` via the `dither/v2` library — **gamma-correct linear RGB + perceptual matching + serpentine**), `packFramebuffer`/`unpackFramebuffer` (4bpp, 2px/byte, high-nibble-first — lossless round-trip), and `encodePNG` (indexed PNG for browser preview). The canvas constants live here.
- `immich.go` — `Client` for the Immich REST API. `listImageAssets` (GET `/api/albums/{id}`, keeps only `type=="IMAGE"`) and `downloadPreview` (GET `/api/assets/{id}/thumbnail?size=preview`, decoded via `image.Decode`). We fetch the **preview, not the original**, on purpose: Immich previews have EXIF orientation baked in (the std-lib JPEG decoder ignores EXIF, so originals come out sideways), are always JPEG/WebP (HEIC-safe), and need only `asset.view` rather than `asset.download`. API key goes in the `x-api-key` header. Endpoint names use the current **plural** form (`/api/albums`, `/api/assets`).
- `app.go` — the `App` interface (`Name`/`Refresh`/`Regenerate`/`Current`/`Previous`/`LastErr`) and the embeddable `imageCache` (RWMutex + keep-last-good-on-error). Image payloads are opaque `[]byte` (the packed framebuffer). It holds both the current image and the **previous** one. `refresh` builds synchronously (startup); `regenerate` rebuilds in a **background goroutine** and is **single-flight** (a no-op if one's already running) — on success the current image shifts to `prevJpeg` and the new one becomes current. Both run `build` without holding the lock, so a slow fetch never blocks readers. `imageCache.name` is carried only so the background goroutine can log meaningfully.
- `immich_app.go` — `immichApp` implements `App` by embedding `imageCache`. Its `build` does the random selection + download + compose + dither + pack; each call re-rolls a new random pair, so the post-serve regenerate naturally yields a different image next time.
- `config.go` — YAML config (`config`/`appConfig`/`immichConfig`), `loadConfig` (yaml.v3 with `KnownFields(true)`), `validateApps` (defaults + checks), and `buildApps` — the **type-keyed factory** where new app types are registered.
- `manager.go` — the in-memory `manager` (base ctx, ordered apps, active index, rotate flag) and all HTTP handlers + aggregated `/healthz`. `writeImage` **content-negotiates** the response: raw framebuffer (`application/octet-stream` + `X-Inky-*` headers) by default, or an indexed PNG when `Accept` contains `text/html` / `?format=png` (`?format=raw` forces the framebuffer). **Serve-then-regenerate:** most serving methods read the served app's `Current()` and then call `Regenerate(m.ctx)` (background, single-flight). The two pure reads are `currentImage` (returns `Current()`, no regen) and `prevImage` (returns `Previous()`, falling back to `Current()` when there's no previous yet, no regen). **Locking discipline:** `mu` guards only `active`/`rotate`; the manager never holds `mu` across an app call — it captures the app under the lock, releases, then reads `Current()`/`Previous()` and triggers `Regenerate` (which returns immediately). `m.ctx` is the server root context, so background builds are cancelled on shutdown.
- `main.go` — lifecycle only: `-config` flag, `loadConfig`→`buildApps`, build every app once synchronously (fail fast), `newManager(ctx, …)`, mux wiring, graceful shutdown. **No refresh timer** — regeneration is driven by serves, not a ticker.

Behavioural decisions already settled (keep them consistent):

- **Selection (immich):** any two *distinct* random `IMAGE` assets (videos excluded); orientation is not filtered. Errors if the album has fewer than 2 images.
- **Fit:** crop-to-fill each half (no distortion, no letterbox). All resize math is in `compose.go`.
- **Rotation:** `/image` with rotate on serves the active app and *then* advances (serve-then-advance). With rotate off it never advances.
- **Within-app nav:** `/next-image` serves the active app's cached image and regenerates it (each fetch shows a fresh one); `/current-image` returns the current image without regenerating; `/prev-image` returns the stored *previous* image (the one replaced by the last regeneration) without regenerating, falling back to current before any regeneration has happened. Only the single most recent previous image is kept — there is no deeper history.
- **Refresh model:** there is **no timer**. Each app caches one image (built once synchronously at startup — fail fast on misconfig, exits non-zero if *any* app fails). Serving an app's image triggers a background, single-flight regeneration of that app so the next serve is fresh. A failed regeneration keeps the last good image rather than blanking the frame. `/healthz` is 503 only if *no* app has an image yet.
- **HTTP:** all endpoints are `GET` (simplest for the frame firmware); action endpoints return the resulting image so the frame renders in one round-trip.
- **Output format:** **never JPEG** — lossy compression would smear the dithered pixels and shift colours off-palette. The server dithers to the 7-colour palette and sends a raw 4bpp framebuffer (or a lossless indexed PNG for browsers). Dithering (algorithm chosen by the `dither` config key: `floyd-steinberg` default or `atkinson`) is done by the `dither/v2` library in gamma-correct linear RGB with perceptual colour matching. The palette/PEN index order and framebuffer packing are a firmware contract — see `README.md`.

## Conventions

- Module path: `github.com/gouthamve/inkyframe-server` (matches the on-disk GOPATH location).
- Configuration is a **single YAML file** via `-config` (default `config.yaml`); there are **no environment variables**. `config.yaml` is gitignored (holds the API key); commit `config.example.yaml`. Never commit or log the API key.
- Std-lib first, but a few well-scoped deps earn their place. External dependencies: `golang.org/x/image` (`/draw` for CatmullRom resampling — the std lib has no quality resizer — and `/webp` to decode WebP previews), `gopkg.in/yaml.v3` (config), and `github.com/makeworld-the-better-one/dither/v2` (gamma-correct, perceptual error-diffusion dithering — pure Go, no CGO, no transitive runtime deps, MPL-2.0; justified by output quality on a sparse 7-colour palette). PNG/colour use std lib (`image/png`, `image/color`). Don't add more without a strong reason; fetching pre-rendered previews keeps us out of the HEIC/CGO-decoder hole entirely.
