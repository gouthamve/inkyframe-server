# inkyframe-server

A small, stateful HTTP server that feeds an **inkyframe** — a 7.3" 800×480 e-ink
photo frame with 5 buttons (rp2040w). The frame wakes periodically, requests an
image, and renders it. The server decides which image to show.

The server runs a configurable set of **apps**. Each app keeps its own image
fresh in the background; the server tracks an **active app** and a **rotate**
flag, and exposes endpoints that map to the frame's buttons.

For now there is one app type, **immich**: it pulls two random images from an
[Immich](https://immich.app) album, stitches them side-by-side, **dithers** the
result to the frame's 7-colour palette on the server, and serves it as a packed
**800×480 4bpp framebuffer** (or an indexed PNG for browsers — see
[Firmware contract](#firmware-contract)).

## How it works

```
                ┌─ app: immich ─▶ [cached 800×480 4bpp framebuffer]
config.yaml ──▶ ├─ app: …               ▲   │
                └─ app: …                │   ▼
            manager (active app + rotate flag) ──▶ GET /image
                    serve cached image ──┘   └─▶ regenerate in background
```

- Each app holds a **cached image** (built once at startup) plus the **previous**
  one. Serving an app's image returns the cached copy instantly and then triggers
  a **background regeneration** of that app: the current image shifts to
  "previous" and the freshly built one becomes current, so the next serve shows a
  fresh image. There is no timer — images only regenerate after they're served.
  Regeneration is single-flight (overlapping serves don't pile up) and keeps the
  last good image if a rebuild fails, rather than blanking the frame.
- `GET /image` returns the active app's cached image. If **rotate** is on, it
  serves the active app and then advances the pointer, so the next request shows
  the next app. If rotate is off, it always returns the active app.
- The button endpoints change state **and return the resulting image**, so the
  frame renders in a single round-trip.

The immich app fetches Immich's **preview** endpoint
(`/api/assets/{id}/thumbnail?size=preview`) rather than originals: previews have
the camera's EXIF orientation already baked in (portrait shots come out upright),
are JPEG/WebP even when the original is HEIC, and only need the `asset.view`
permission. Each image is **cropped to fill** its 400×480 half (centered,
aspect-preserving — no distortion, no letterbox). The composite is then dithered
(Floyd–Steinberg or Atkinson, configurable) to the 7-colour palette and packed
into the framebuffer. Videos are ignored; an immich album must contain at least
2 images.

## Configuration

All configuration is a single YAML file, passed via `-config` (default
`config.yaml`). There are no environment variables. See
[`config.example.yaml`](config.example.yaml):

```yaml
listen_addr: ":8080"      # HTTP listen address (default :8080)
rotate: true              # if true, /image advances to the next app each request (default true)
dither: floyd-steinberg   # dithering algorithm: floyd-steinberg (default) or atkinson

apps:
  # The default app is the first one listed.
  - name: family-photos
    type: immich
    immich:
      url: https://immich.example.com   # Immich server base URL
      api_key: your-api-key             # needs asset.view + album read; keep secret
      album_id: your-album-uuid         # album to pull images from
      jpeg_quality: 85                  # output JPEG quality 1-100 (default 85)
```

Top-level keys: `listen_addr`, `rotate`, `dither`, and a non-empty `apps` list.
`dither` chooses the error-diffusion algorithm applied to every app's image:
`floyd-steinberg` (default; smooth gradients) or `atkinson` (higher contrast,
often cleaner on e-ink). Each app needs a unique `name` and a `type`; an `immich`
app needs a `immich:` block with `url`, `api_key`, and `album_id`. Unknown keys
are rejected so typos surface immediately.

## Run

```sh
cp config.example.yaml config.yaml   # then edit it
go run . -config config.yaml
# or: go build -o inkyframe-server . && ./inkyframe-server -config config.yaml
```

The server builds every app's first image synchronously at startup and exits
non-zero if any fails (so misconfiguration is caught immediately).

```sh
# Device: raw 4bpp framebuffer (exactly 192000 bytes for 800×480).
curl -s localhost:8080/image -o frame.bin && wc -c frame.bin

# Browser-friendly PNG (Accept sniff, or force it with ?format=png).
curl -s -H 'Accept: text/html' localhost:8080/image -o out.png && file out.png
```

## Docker

A multi-arch image (`linux/amd64` + `linux/arm64`) is built and pushed to the
GitHub Container Registry by CI — on every push to `main` and on `v*` tags:

```sh
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/inkyframe/config.yaml:ro" \
  ghcr.io/gouthamve/inkyframe-server:main
```

It is a static [`distroless`](https://github.com/GoogleContainerTools/distroless)
build that runs as a non-root user and reads its config from
`/etc/inkyframe/config.yaml`. Mount your own config there (it holds the API key,
so it is never baked into the image); pass a different `-config` path if you
mount it elsewhere. Available tags: `main` (rolling — latest commit on `main`),
`main-<commit>` (immutable — pins a specific `main` build),
and `vX.Y.Z` / `vX.Y` / `latest` (release tags).

## Endpoints

All endpoints are `GET` (simplest for the frame firmware) and the action
endpoints return the resulting image directly. The output format is
**content-negotiated**: by default the server returns the raw 4bpp framebuffer
(`Content-Type: application/octet-stream`, with `X-Inky-Format`/`-Width`/`-Height`/`-Bpp`
headers); a request whose `Accept` header contains `text/html` (i.e. a browser),
or `?format=png`, gets an indexed `image/png`; `?format=raw` forces the framebuffer.
The `X-App` response header names the app that produced the image. See
[Firmware contract](#firmware-contract) for the exact framebuffer format.

| Path | Button | Description |
|------|--------|-------------|
| `GET /image` (also `GET /`) | — | Active app's image; advances the active app if rotate is on |
| `GET /next-app` | Next App | Advance to the next app; return its image |
| `GET /prev-app` | Previous App | Move to the previous app; return its image |
| `GET /next-image` | Next Image | Return the active app's image (and regenerate it) — each fetch shows a fresh one |
| `GET /prev-image` | Previous Image | Return the active app's **previous** image (the one before the current), without regenerating |
| `GET /current-image` | — | Return the active app's current image **without** regenerating it |
| `GET /toggle-rotate` | Toggle Rotate | Flip rotation on/off; return the active app's image |
| `GET /healthz` | — | Per-app status + `rotate=…`; 503 only if no app has an image yet |

Each app keeps both its current image and the previous one (the image replaced by
the last regeneration). `GET /prev-image` and `GET /current-image` are pure reads;
every other image endpoint regenerates the served app's image in the background
after responding, so the next fetch shows something new. Before the first
regeneration there is no previous image, so `GET /prev-image` returns the current
one.

## Firmware contract

The server dithers on the server side and sends the device a **pre-quantised**
image, so the firmware does no decoding or dithering — it just blits the bytes.
The format below is a contract between this server and the frame firmware.

**Palette / PEN index.** The image is dithered to a 7-colour palette. Each pixel
is a palette **index**; the index order is the Inky Frame PEN numbering and must
match the firmware:

| Index | Colour |
|-------|--------|
| 0 | black |
| 1 | white |
| 2 | green |
| 3 | blue |
| 4 | red |
| 5 | yellow |
| 6 | orange |

PEN 7 ("clean"/taupe) is never emitted. The RGB values the server dithers against
(in `inkyPalette`, `compose.go`) are tunable to the panel's measured output for
better-looking results — **only the index order is the contract**, not the RGB.

**Framebuffer packing** (`Content-Type: application/octet-stream`). 4 bits per
pixel, row-major, two pixels per byte, the **first (left) pixel of each pair in
the high nibble**: `byte = (idxLeft << 4) | idxRight`. For 800×480 that is exactly
**192,000 bytes** (400 bytes/row, no padding). The response also carries
`X-Inky-Format: inky7-4bpp`, `X-Inky-Width: 800`, `X-Inky-Height: 480`,
`X-Inky-Bpp: 4` for in-band sanity checks.

**Browser preview** (`Content-Type: image/png`). A request whose `Accept` header
contains `text/html`, or `?format=png`, gets the same image as an indexed PNG
(decoded losslessly from the framebuffer). `?format=raw` forces the framebuffer
regardless of `Accept`.

**Dithering** is done by the [`dither/v2`](https://github.com/makeworld-the-better-one/dither)
library, which diffuses error in **linear (gamma-correct) RGB** with perceptually
weighted colour matching and serpentine scanning — important for a sparse 7-colour
palette. Algorithm is selectable via the top-level `dither` config key.

## Notes & limitations

- `config.yaml` holds the Immich API key in plaintext — it is gitignored; commit
  `config.example.yaml` instead.
- After startup, if a background regeneration fails (e.g. Immich is briefly
  unreachable) the **last good image is kept** rather than blanking the display.
- Images regenerate only after being served, so a freshly-served image and the
  one prepared for the next request can be at most one round-trip apart.
- Source formats like HEIC work fine because images come from Immich's
  pre-rendered previews (JPEG/WebP) — no client-side HEIC/CGO decoder is needed.
- Output is **never JPEG**: lossy compression would smear the dithered pixels and
  shift colours off the 7-colour palette, so the server sends the raw framebuffer
  (or a lossless indexed PNG for browsers) instead.
- The endpoints have **no authentication**; run the server on a trusted network.

## Development

```sh
go test ./...      # compose pipeline + manager state + config parsing
go test -race ./... # manager concurrency
go build ./...
go vet ./...
```
