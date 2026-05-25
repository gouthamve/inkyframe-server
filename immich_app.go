package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// immichApp is an App backed by an Immich album: each build picks two distinct
// random IMAGE assets, downloads their previews, and stitches them into one
// 800×480 composite. Because every build re-rolls the pair, the regenerate
// triggered after each serve naturally produces a different image next time.
type immichApp struct {
	client     *Client
	albumID    string
	algo       ditherAlgo // dithering algorithm for the composed image
	imageCache            // embedded: refresh/regenerate/current/lastErr + locking
}

func newImmichApp(name string, cfg immichConfig, algo ditherAlgo) *immichApp {
	a := &immichApp{
		client:  NewClient(cfg.URL, cfg.APIKey),
		albumID: cfg.AlbumID,
		algo:    algo,
	}
	a.imageCache.name = name
	a.imageCache.build = a.build
	return a
}

// build selects two distinct random images from the album, downloads their
// previews, composes them side by side, dithers the result to the 7-colour
// palette, and packs it into the panel's 4bpp framebuffer.
func (a *immichApp) build(ctx context.Context) ([]byte, error) {
	images, err := a.client.listImageAssets(ctx, a.albumID)
	if err != nil {
		return nil, err
	}
	if len(images) < 2 {
		return nil, fmt.Errorf("album %s has %d image(s), need at least 2", a.albumID, len(images))
	}

	// Pick two distinct images at random.
	i := rand.IntN(len(images))
	j := rand.IntN(len(images) - 1)
	if j >= i {
		j++
	}

	left, err := a.client.downloadPreview(ctx, images[i].ID)
	if err != nil {
		return nil, err
	}
	right, err := a.client.downloadPreview(ctx, images[j].ID)
	if err != nil {
		return nil, err
	}

	return packFramebuffer(ditherImage(compose(left, right), a.algo)), nil
}

func (a *immichApp) Name() string                      { return a.imageCache.name }
func (a *immichApp) Refresh(ctx context.Context) error { return a.imageCache.refresh(ctx) }
func (a *immichApp) Regenerate(ctx context.Context)    { a.imageCache.regenerate(ctx) }
func (a *immichApp) Current() ([]byte, time.Time)      { return a.imageCache.current() }
func (a *immichApp) Previous() ([]byte, time.Time)     { return a.imageCache.previous() }
func (a *immichApp) LastErr() error                    { return a.imageCache.lastErr() }
