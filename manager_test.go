package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeApp is a minimal App with canned bytes and no network. Regenerate just
// counts calls, so tests can assert which serves trigger a regeneration.
type fakeApp struct {
	name string
	img  []byte
	prev []byte

	mu     sync.Mutex
	regens int
}

func (a *fakeApp) Name() string                  { return a.name }
func (a *fakeApp) Refresh(context.Context) error { return nil }
func (a *fakeApp) Current() ([]byte, time.Time)  { return a.img, time.Unix(0, 0) }
func (a *fakeApp) Previous() ([]byte, time.Time) { return a.prev, time.Unix(0, 0) }
func (a *fakeApp) LastErr() error                { return nil }

func (a *fakeApp) Regenerate(context.Context) {
	a.mu.Lock()
	a.regens++
	a.mu.Unlock()
}

func (a *fakeApp) regenCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.regens
}

func threeApps() []App {
	return []App{
		&fakeApp{name: "a", img: []byte("A")},
		&fakeApp{name: "b", img: []byte("B")},
		&fakeApp{name: "c", img: []byte("C")},
	}
}

func newTestManager(apps []App, rotate bool) *manager {
	return newManager(context.Background(), apps, rotate)
}

func TestCurrentRotateOff(t *testing.T) {
	apps := threeApps()
	m := newTestManager(apps, false)
	for i := 0; i < 3; i++ {
		app, _, _ := m.current()
		if app.Name() != "a" {
			t.Fatalf("call %d: rotate off should always serve 'a', got %q", i, app.Name())
		}
	}
	// Each serve regenerates the served app.
	if n := apps[0].(*fakeApp).regenCount(); n != 3 {
		t.Fatalf("expected 'a' regenerated 3 times, got %d", n)
	}
}

func TestCurrentRotateOnServesThenAdvances(t *testing.T) {
	m := newTestManager(threeApps(), true)
	want := []string{"a", "b", "c", "a"} // serve current, then advance
	for i, w := range want {
		app, _, _ := m.current()
		if app.Name() != w {
			t.Fatalf("call %d: got %q, want %q", i, app.Name(), w)
		}
	}
}

func TestNextPrevAppWraparound(t *testing.T) {
	m := newTestManager(threeApps(), false)

	if app, _, _ := m.nextApp(); app.Name() != "b" {
		t.Fatalf("nextApp: got %q, want b", app.Name())
	}
	if app, _, _ := m.prevApp(); app.Name() != "a" {
		t.Fatalf("prevApp: got %q, want a", app.Name())
	}
	// prev from index 0 wraps to the last app.
	if app, _, _ := m.prevApp(); app.Name() != "c" {
		t.Fatalf("prevApp wraparound: got %q, want c", app.Name())
	}
	// next from last wraps to the first.
	if app, _, _ := m.nextApp(); app.Name() != "a" {
		t.Fatalf("nextApp wraparound: got %q, want a", app.Name())
	}
}

func TestNextImageServesAndRegenerates(t *testing.T) {
	apps := threeApps()
	m := newTestManager(apps, false)

	app, img, _ := m.nextImage()
	if app.Name() != "a" || string(img) != "A" {
		t.Fatalf("nextImage: got (%q, %q), want (a, A)", app.Name(), img)
	}
	if n := apps[0].(*fakeApp).regenCount(); n != 1 {
		t.Fatalf("nextImage should regenerate once, got %d", n)
	}
	// It must not change the active app.
	if app2, _, _ := m.current(); app2.Name() != "a" {
		t.Fatalf("nextImage changed active app: now %q, want a", app2.Name())
	}
}

func TestCurrentImageNoRegenerate(t *testing.T) {
	apps := threeApps()
	m := newTestManager(apps, false)
	app, img, _ := m.currentImage()
	if app.Name() != "a" || string(img) != "A" {
		t.Fatalf("currentImage: got (%q, %q), want (a, A)", app.Name(), img)
	}
	if n := apps[0].(*fakeApp).regenCount(); n != 0 {
		t.Fatalf("currentImage must not regenerate, got %d", n)
	}
	// active must be unchanged.
	if app2, _, _ := m.currentImage(); app2.Name() != "a" {
		t.Fatalf("currentImage moved active: now %q, want a", app2.Name())
	}
}

func TestPrevImageReturnsPreviousWithoutRegenerating(t *testing.T) {
	apps := []App{
		&fakeApp{name: "a", img: []byte("A2"), prev: []byte("A1")},
		&fakeApp{name: "b", img: []byte("B"), prev: []byte("Bprev")},
	}
	m := newTestManager(apps, false)
	m.nextApp() // active -> b, regenerates b once

	bRegens := apps[1].(*fakeApp).regenCount()
	app, img, _ := m.prevImage()
	if app.Name() != "b" || string(img) != "Bprev" {
		t.Fatalf("prevImage: got (%q, %q), want (b, Bprev)", app.Name(), img)
	}
	if n := apps[1].(*fakeApp).regenCount(); n != bRegens {
		t.Fatalf("prevImage must not regenerate: regen count changed %d -> %d", bRegens, n)
	}
	// active must be unchanged.
	if app2, _, _ := m.prevImage(); app2.Name() != "b" {
		t.Fatalf("prevImage moved active: now %q, want b", app2.Name())
	}
}

func TestPrevImageFallsBackToCurrent(t *testing.T) {
	// No previous image yet (prev is nil): prevImage falls back to current.
	apps := []App{&fakeApp{name: "a", img: []byte("A")}}
	m := newTestManager(apps, false)
	app, img, _ := m.prevImage()
	if app.Name() != "a" || string(img) != "A" {
		t.Fatalf("prevImage fallback: got (%q, %q), want (a, A)", app.Name(), img)
	}
}

func TestToggleRotate(t *testing.T) {
	apps := threeApps()
	m := newTestManager(apps, false)
	app, _, _, rotate := m.toggleRotate()
	if !rotate {
		t.Fatal("toggleRotate: expected rotate=true after first toggle")
	}
	if app.Name() != "a" {
		t.Fatalf("toggleRotate: active app should be unchanged 'a', got %q", app.Name())
	}
	if n := apps[0].(*fakeApp).regenCount(); n != 1 {
		t.Fatalf("toggleRotate should regenerate the active app once, got %d", n)
	}
	if _, _, _, rotate := m.toggleRotate(); rotate {
		t.Fatal("toggleRotate: expected rotate=false after second toggle")
	}
}

// TestManagerConcurrency hammers the manager from many goroutines under -race
// to confirm no data races and that the active index stays valid.
func TestManagerConcurrency(t *testing.T) {
	m := newTestManager(threeApps(), true)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				switch j % 7 {
				case 0:
					m.current()
				case 1:
					m.nextApp()
				case 2:
					m.prevApp()
				case 3:
					m.nextImage()
				case 4:
					m.currentImage()
				case 5:
					m.prevImage()
				case 6:
					m.toggleRotate()
				}
			}
		}()
	}
	wg.Wait()
	// Active index must remain in range after the churn.
	if m.active < 0 || m.active >= len(m.apps) {
		t.Fatalf("active index out of range: %d", m.active)
	}
}
