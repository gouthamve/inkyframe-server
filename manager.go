package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// manager holds the in-memory frame state: an ordered list of apps, the active
// app index, and the rotate flag. It is the single owner of that state and the
// home of the HTTP handlers.
//
// Each app caches its current image. The manager serves that cached image
// instantly and then triggers a background Regenerate on the served app, so the
// next serve shows a fresh image. prevImage is the one exception: it redraws the
// current cached image without regenerating.
//
// Locking discipline: mu guards only the active and rotate scalars. The manager
// never calls a blocking operation while holding mu; it captures the app under
// the lock, releases, then reads Current (and Regenerate returns immediately,
// doing its build in a background goroutine). Each app's bytes are guarded by
// its own imageCache mutex.
type manager struct {
	ctx    context.Context // base context for background regeneration
	mu     sync.Mutex
	apps   []App // immutable after construction; index order == config order
	active int
	rotate bool
}

func newManager(ctx context.Context, apps []App, rotate bool) *manager {
	return &manager{ctx: ctx, apps: apps, rotate: rotate}
}

// current serves GET /image: it returns the active app's image and moves that
// app forward for next time. For a Navigator app (a movie) it steps to the next
// frame and returns it synchronously (clamped at the last); for other apps it
// returns the cached image and regenerates it in the background. If rotation is
// on it then advances the active pointer (the app being served is captured
// before advancing, so the next request shows the next app); with rotation off
// it doesn't advance the pointer.
func (m *manager) current() (App, []byte, time.Time) {
	m.mu.Lock()
	app := m.apps[m.active]
	if m.rotate {
		m.active = (m.active + 1) % len(m.apps)
	}
	m.mu.Unlock()
	if nav, ok := app.(Navigator); ok {
		img, at := nav.Next()
		return app, img, at
	}
	img, at := app.Current()
	app.Regenerate(m.ctx)
	return app, img, at
}

// nextApp advances the active app, returns its cached image, and regenerates it.
func (m *manager) nextApp() (App, []byte, time.Time) {
	m.mu.Lock()
	m.active = (m.active + 1) % len(m.apps)
	app := m.apps[m.active]
	m.mu.Unlock()
	img, at := app.Current()
	app.Regenerate(m.ctx)
	return app, img, at
}

// prevApp moves the active app back one, returns its cached image, and
// regenerates it.
func (m *manager) prevApp() (App, []byte, time.Time) {
	m.mu.Lock()
	m.active = (m.active - 1 + len(m.apps)) % len(m.apps)
	app := m.apps[m.active]
	m.mu.Unlock()
	img, at := app.Current()
	app.Regenerate(m.ctx)
	return app, img, at
}

// nextImage advances the active app's image. For a Navigator app (e.g. a movie)
// it steps to the next frame and returns it synchronously. Otherwise it serves
// the cached image and regenerates it — because every serve regenerates, the
// cached image is already the "next" one. It changes neither active nor rotate.
func (m *manager) nextImage() (App, []byte, time.Time) {
	m.mu.Lock()
	app := m.apps[m.active]
	m.mu.Unlock()
	if nav, ok := app.(Navigator); ok {
		img, at := nav.Next()
		return app, img, at
	}
	img, at := app.Current()
	app.Regenerate(m.ctx)
	return app, img, at
}

// currentImage returns the active app's current cached image WITHOUT
// regenerating, so the same image can be re-fetched.
func (m *manager) currentImage() (App, []byte, time.Time) {
	m.mu.Lock()
	app := m.apps[m.active]
	m.mu.Unlock()
	img, at := app.Current()
	return app, img, at
}

// prevImage steps the active app's image backwards. For a Navigator app (e.g. a
// movie) it steps to the previous frame and returns it synchronously. Otherwise
// it returns the app's previous image (the one replaced by the most recent
// regeneration) WITHOUT regenerating; before any regeneration has happened there
// is no previous image, so it falls back to the current one.
func (m *manager) prevImage() (App, []byte, time.Time) {
	m.mu.Lock()
	app := m.apps[m.active]
	m.mu.Unlock()
	if nav, ok := app.(Navigator); ok {
		img, at := nav.Prev()
		return app, img, at
	}
	img, at := app.Previous()
	if img == nil {
		img, at = app.Current()
	}
	return app, img, at
}

// toggleRotate flips the rotate flag and returns the active app's cached image
// (regenerating it) along with the new flag value.
func (m *manager) toggleRotate() (App, []byte, time.Time, bool) {
	m.mu.Lock()
	m.rotate = !m.rotate
	rotate := m.rotate
	app := m.apps[m.active]
	m.mu.Unlock()
	img, at := app.Current()
	app.Regenerate(m.ctx)
	return app, img, at, rotate
}

// writeImage writes the cached 4bpp framebuffer with frame-friendly headers, or
// 503 if no image has been built yet. It content-negotiates the format: the
// device gets the raw framebuffer (application/octet-stream); a browser gets an
// indexed PNG decoded from it. The format is chosen by an explicit ?format=raw|png
// query, else by sniffing the Accept header for text/html (browsers). X-App names
// the app that produced it, for debugging.
func writeImage(w http.ResponseWriter, r *http.Request, app App, fb []byte, builtAt time.Time) {
	if fb == nil {
		http.Error(w, "no image available yet", http.StatusServiceUnavailable)
		return
	}

	wantPNG := false
	switch r.URL.Query().Get("format") {
	case "png":
		wantPNG = true
	case "raw":
		wantPNG = false
	default:
		wantPNG = strings.Contains(r.Header.Get("Accept"), "text/html")
	}

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Last-Modified", builtAt.UTC().Format(http.TimeFormat))
	w.Header().Set("X-App", app.Name())

	if wantPNG {
		out, err := encodePNG(unpackFramebuffer(fb))
		if err != nil {
			http.Error(w, "encode png: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(out)
		return
	}

	// Raw 4bpp framebuffer for the frame firmware. The X-Inky-* headers document
	// the format in-band; see the firmware contract in README.md.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Inky-Format", "inky7-4bpp")
	w.Header().Set("X-Inky-Width", strconv.Itoa(canvasWidth))
	w.Header().Set("X-Inky-Height", strconv.Itoa(canvasHeight))
	w.Header().Set("X-Inky-Bpp", "4")
	_, _ = w.Write(fb)
}

func (m *manager) handleImage(w http.ResponseWriter, r *http.Request) {
	app, fb, at := m.current()
	writeImage(w, r, app, fb, at)
}

func (m *manager) handleNextApp(w http.ResponseWriter, r *http.Request) {
	app, fb, at := m.nextApp()
	writeImage(w, r, app, fb, at)
}

func (m *manager) handlePrevApp(w http.ResponseWriter, r *http.Request) {
	app, fb, at := m.prevApp()
	writeImage(w, r, app, fb, at)
}

func (m *manager) handleNextImage(w http.ResponseWriter, r *http.Request) {
	app, fb, at := m.nextImage()
	writeImage(w, r, app, fb, at)
}

func (m *manager) handleCurrentImage(w http.ResponseWriter, r *http.Request) {
	app, fb, at := m.currentImage()
	writeImage(w, r, app, fb, at)
}

func (m *manager) handlePrevImage(w http.ResponseWriter, r *http.Request) {
	app, fb, at := m.prevImage()
	writeImage(w, r, app, fb, at)
}

func (m *manager) handleToggleRotate(w http.ResponseWriter, r *http.Request) {
	app, fb, at, rotate := m.toggleRotate()
	log.Printf("rotate toggled to %v (active app %q)", rotate, app.Name())
	writeImage(w, r, app, fb, at)
}

// handleHealth aggregates over all apps. It returns 503 only when no app has
// any image yet (nothing to serve); otherwise 200, reporting each app's state
// and marking the active one. An app serving a cached image after a failed
// refresh is reported as degraded but still counts as healthy.
func (m *manager) handleHealth(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	active := m.active
	rotate := m.rotate
	m.mu.Unlock()

	anyImage := false
	var b strings.Builder
	for i, app := range m.apps {
		img, _ := app.Current()
		err := app.LastErr()
		marker := ""
		if i == active {
			marker = " (active)"
		}
		switch {
		case img == nil && err != nil:
			fmt.Fprintf(&b, "%s%s: no image yet, last error: %v\n", app.Name(), marker, err)
		case img == nil:
			fmt.Fprintf(&b, "%s%s: no image yet\n", app.Name(), marker)
		case err != nil:
			anyImage = true
			fmt.Fprintf(&b, "%s%s: ok (serving cached; last refresh failed: %v)\n", app.Name(), marker, err)
		default:
			anyImage = true
			fmt.Fprintf(&b, "%s%s: ok\n", app.Name(), marker)
		}
	}

	if !anyImage {
		http.Error(w, b.String(), http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, "rotate=%v\n%s", rotate, b.String())
}
