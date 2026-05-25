package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// App is a renderable source of 800×480 images for the frame (a packed 4bpp
// framebuffer; see compose.go). Each app holds one cached image. The manager
// serves that cached image instantly and then asks the app to Regenerate, so the
// next serve shows a fresh image.
type App interface {
	// Name is the human-readable identifier from config (also used in logs).
	Name() string

	// Refresh builds the image synchronously and stores it. It is used once at
	// startup so misconfiguration fails fast and there's an image to serve.
	Refresh(ctx context.Context) error

	// Regenerate rebuilds the image in the background and swaps it in once
	// ready, so the next serve gets a fresh image. It returns immediately and
	// is single-flight (a no-op if a regeneration is already running). On
	// failure it keeps the last good image rather than blanking the frame.
	Regenerate(ctx context.Context)

	// Current returns the latest good image and the time it was built. It
	// returns (nil, zero) if no image has been built yet.
	Current() (image []byte, builtAt time.Time)

	// Previous returns the image that was current before the most recent
	// regeneration replaced it, and the time it was built. It returns
	// (nil, zero) if no regeneration has happened yet.
	Previous() (image []byte, builtAt time.Time)

	// LastErr returns the error from the most recent build, or nil. It is used
	// by /healthz to report a degraded-but-serving state.
	LastErr() error
}

// imageCache holds the most recent good image for one app along with the build
// function that produces it. It implements the keep-last-good-image-on-failure
// behaviour and is safe for concurrent use. Apps embed it and supply build.
type imageCache struct {
	name  string // for log messages
	build func(context.Context) ([]byte, error)

	mu           sync.RWMutex
	img          []byte // current image
	builtAt      time.Time
	prevImg      []byte // image replaced by the most recent regeneration
	prevBuiltAt  time.Time
	buildErr     error
	regenerating bool
}

// refresh builds the image synchronously and stores it. On error the previous
// image is kept so a transient failure doesn't blank the frame.
func (c *imageCache) refresh(ctx context.Context) error {
	img, err := c.build(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buildErr = err
	if err != nil {
		return err
	}
	c.img = img
	c.builtAt = time.Now()
	return nil
}

// regenerate rebuilds the image in a background goroutine and swaps it in once
// ready. It is single-flight: if a regeneration is already running it does
// nothing. The build runs without holding the lock, so it never blocks readers.
func (c *imageCache) regenerate(ctx context.Context) {
	c.mu.Lock()
	if c.regenerating {
		c.mu.Unlock()
		return
	}
	c.regenerating = true
	c.mu.Unlock()

	go func() {
		img, err := c.build(ctx)
		c.mu.Lock()
		c.regenerating = false
		c.buildErr = err
		if err == nil {
			// The image being replaced becomes the previous one.
			c.prevImg, c.prevBuiltAt = c.img, c.builtAt
			c.img, c.builtAt = img, time.Now()
		}
		c.mu.Unlock()
		if err != nil {
			log.Printf("background regeneration failed for %q (keeping previous image): %v", c.name, err)
		}
	}()
}

func (c *imageCache) current() ([]byte, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.img, c.builtAt
}

func (c *imageCache) previous() ([]byte, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.prevImg, c.prevBuiltAt
}

func (c *imageCache) lastErr() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.buildErr
}
