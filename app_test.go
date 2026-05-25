package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond until it's true or the timeout elapses.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestImageCacheShiftsPreviousOnRegenerate(t *testing.T) {
	var n int32
	c := imageCache{
		name: "test",
		build: func(context.Context) ([]byte, error) {
			return []byte{byte(atomic.AddInt32(&n, 1))}, nil
		},
	}

	// Startup build: current is the first image, no previous yet.
	if err := c.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if cur, _ := c.current(); len(cur) != 1 || cur[0] != 1 {
		t.Fatalf("after refresh current = %v, want {1}", cur)
	}
	if prev, _ := c.previous(); prev != nil {
		t.Fatalf("after refresh previous = %v, want nil", prev)
	}

	// First regeneration: current {1} shifts to previous, {2} becomes current.
	c.regenerate(context.Background())
	waitFor(t, func() bool { p, _ := c.previous(); return p != nil })
	if cur, _ := c.current(); cur[0] != 2 {
		t.Fatalf("after 1st regen current = %v, want {2}", cur)
	}
	if prev, _ := c.previous(); prev[0] != 1 {
		t.Fatalf("after 1st regen previous = %v, want {1}", prev)
	}

	// Second regeneration: {2} shifts to previous, {3} becomes current.
	c.regenerate(context.Background())
	waitFor(t, func() bool { p, _ := c.previous(); return p[0] == 2 })
	if cur, _ := c.current(); cur[0] != 3 {
		t.Fatalf("after 2nd regen current = %v, want {3}", cur)
	}
}

func TestImageCacheKeepsImagesOnRegenerateError(t *testing.T) {
	var fail atomic.Bool
	var calls int32
	c := imageCache{
		name: "test",
		build: func(context.Context) ([]byte, error) {
			atomic.AddInt32(&calls, 1)
			if fail.Load() {
				return nil, errors.New("boom")
			}
			return []byte{1}, nil
		},
	}
	if err := c.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// A failing regeneration must keep the current image and record the error.
	fail.Store(true)
	before := atomic.LoadInt32(&calls)
	c.regenerate(context.Background())
	waitFor(t, func() bool { return c.lastErr() != nil })
	if cur, _ := c.current(); len(cur) != 1 || cur[0] != 1 {
		t.Fatalf("current changed after failed regen: %v", cur)
	}
	if atomic.LoadInt32(&calls) <= before {
		t.Fatal("build was not invoked by regenerate")
	}
}
