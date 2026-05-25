package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// rawFrame builds one rgb24 frame of the canvas size filled with a solid colour.
func rawFrame(r, g, b byte) []byte {
	f := make([]byte, rawFrameSize)
	for i := 0; i < canvasWidth*canvasHeight; i++ {
		f[i*3+0] = r
		f[i*3+1] = g
		f[i*3+2] = b
	}
	return f
}

func TestReadFrame(t *testing.T) {
	dz := newDitherer(ditherFloydSteinberg)
	wantLen := canvasWidth * canvasHeight / 2

	// Solid frames in colours that are exact palette entries dither with zero
	// error to diffuse, so every pixel maps to that single PEN index and the
	// packed bytes are fully deterministic: black=PEN 0 (0x00), red=PEN 4
	// (0x44), white=PEN 1 (0x11). Each byte packs two same-index pixels.
	for _, tc := range []struct {
		name    string
		r, g, b byte
		want    byte
	}{
		{"black", 0, 0, 0, 0x00},
		{"red", 0xFF, 0, 0, 0x44},
		{"white", 0xFF, 0xFF, 0xFF, 0x11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb, err := readFrame(bytes.NewReader(rawFrame(tc.r, tc.g, tc.b)), dz)
			if err != nil {
				t.Fatalf("readFrame: %v", err)
			}
			if len(fb) != wantLen {
				t.Fatalf("framebuffer is %d bytes, want %d", len(fb), wantLen)
			}
			for j, got := range fb {
				if got != tc.want {
					t.Fatalf("byte %d = %#02x, want %#02x", j, got, tc.want)
				}
			}
		})
	}
}

func TestReadFrameRejectsPartialFrame(t *testing.T) {
	if _, err := readFrame(bytes.NewReader([]byte{1, 2, 3}), newDitherer(ditherFloydSteinberg)); err == nil {
		t.Fatal("expected error for a truncated frame, got nil")
	}
}

func TestParseFrameRate(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"24000/1001", 24000.0 / 1001.0, false},
		{"30/1", 30, false},
		{"25", 25, false},
		{" 50/2 ", 25, false},
		{"", 0, true},
		{"30/0", 0, true},
		{"abc", 0, true},
		{"x/2", 0, true},
	} {
		got, err := parseFrameRate(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseFrameRate(%q): want error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseFrameRate(%q): %v", tc.in, err)
			continue
		}
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("parseFrameRate(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestMovieAppNavigationClamps drives the Navigator logic with an injected
// decoder (no ffmpeg): each "frame" is a one-byte slice equal to its index.
func TestMovieAppNavigationClamps(t *testing.T) {
	a := &movieApp{
		name:        "test",
		totalFrames: 3,
		idx:         0,
		cachedFB:    []byte{0},
		loadedAt:    time.Now(),
	}
	a.decode = func(_ context.Context, idx int) ([]byte, error) { return []byte{byte(idx)}, nil }

	cur := func() byte { img, _ := a.Current(); return img[0] }

	if cur() != 0 {
		t.Fatalf("initial current = %d, want 0", cur())
	}
	// Next advances and clamps at the last frame.
	if img, _ := a.Next(); img[0] != 1 {
		t.Fatalf("next = %d, want 1", img[0])
	}
	if img, _ := a.Next(); img[0] != 2 {
		t.Fatalf("next = %d, want 2", img[0])
	}
	if img, _ := a.Next(); img[0] != 2 {
		t.Fatalf("next past end = %d, want 2 (clamped)", img[0])
	}
	if cur() != 2 {
		t.Fatalf("current after clamping = %d, want 2", cur())
	}
	// Prev steps back and clamps at the first frame.
	if img, _ := a.Prev(); img[0] != 1 {
		t.Fatalf("prev = %d, want 1", img[0])
	}
	if img, _ := a.Prev(); img[0] != 0 {
		t.Fatalf("prev = %d, want 0", img[0])
	}
	if img, _ := a.Prev(); img[0] != 0 {
		t.Fatalf("prev past start = %d, want 0 (clamped)", img[0])
	}
}

// TestMovieAppKeepsFrameOnDecodeError checks a failed decode keeps the current
// frame and index rather than blanking, and records the error.
func TestMovieAppKeepsFrameOnDecodeError(t *testing.T) {
	a := &movieApp{
		name:        "test",
		totalFrames: 5,
		idx:         2,
		cachedFB:    []byte{2},
		loadedAt:    time.Now(),
	}
	a.decode = func(_ context.Context, idx int) ([]byte, error) { return nil, fmt.Errorf("boom") }

	img, _ := a.Next()
	if img[0] != 2 {
		t.Fatalf("kept frame = %d, want 2", img[0])
	}
	if a.idx != 2 {
		t.Fatalf("idx moved to %d, want 2 (unchanged)", a.idx)
	}
	if a.LastErr() == nil {
		t.Fatal("want LastErr set after a decode failure")
	}
}

func TestMovieAppStatePersistence(t *testing.T) {
	dir := t.TempDir()
	sp := filepath.Join(dir, "state.idx")
	a := &movieApp{name: "test", statePath: sp}

	if got := a.readState(); got != 0 {
		t.Fatalf("readState before any write = %d, want 0", got)
	}
	a.persist(42)
	if got := a.readState(); got != 42 {
		t.Fatalf("readState = %d, want 42", got)
	}
	a.persist(7) // overwrite
	if got := a.readState(); got != 7 {
		t.Fatalf("readState after overwrite = %d, want 7", got)
	}

	// With no state file configured, persist is a no-op and readState is 0.
	b := &movieApp{name: "test"}
	b.persist(5)
	if got := b.readState(); got != 0 {
		t.Fatalf("readState with persistence disabled = %d, want 0", got)
	}
}

func TestMovieAppCurrentEmpty(t *testing.T) {
	a := &movieApp{name: "test"}
	if img, _ := a.Current(); img != nil {
		t.Fatalf("current with no frame = %v, want nil", img)
	}
}

// TestMovieAppRefreshWithFFmpeg exercises the real ffmpeg/ffprobe pipeline end
// to end, including resume-on-restart and end clamping. It is skipped when the
// tools aren't installed so the unit suite stays dependency-free.
func TestMovieAppRefreshWithFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "test.mp4")
	gen := exec.Command("ffmpeg", "-y", "-f", "lavfi",
		"-i", "testsrc=duration=2:size=320x240:rate=30",
		"-pix_fmt", "yuv420p", path)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate test video: %v\n%s", err, out)
	}

	statePath := filepath.Join(dir, "state.idx")
	cfg := movieConfig{Path: path, FPS: 5, StateFile: statePath} // 2s × 5fps ≈ 10 frames
	wantLen := canvasWidth * canvasHeight / 2

	a := newMovieApp("test", cfg, newDitherer(ditherFloydSteinberg))
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.totalFrames < 2 {
		t.Fatalf("totalFrames = %d, want >= 2", a.totalFrames)
	}
	cur, _ := a.Current()
	if len(cur) != wantLen {
		t.Fatalf("current frame is %d bytes, want %d", len(cur), wantLen)
	}

	// Next advances to a visibly different frame (testsrc is animated) and
	// persists the new position.
	n1, _ := a.Next()
	if bytes.Equal(n1, cur) {
		t.Fatal("next frame is identical to the first; expected the movie to advance")
	}
	if got := a.readState(); got != 1 {
		t.Fatalf("persisted index = %d, want 1", got)
	}

	// Resume: a fresh app reads the saved index.
	b := newMovieApp("test", cfg, newDitherer(ditherFloydSteinberg))
	if err := b.Refresh(context.Background()); err != nil {
		t.Fatalf("resume refresh: %v", err)
	}
	if b.idx != 1 {
		t.Fatalf("resumed at frame %d, want 1", b.idx)
	}

	// Clamp: a stale, too-large saved index resumes at the last frame.
	if err := os.WriteFile(statePath, []byte("99999"), 0o644); err != nil {
		t.Fatalf("write stale state: %v", err)
	}
	c := newMovieApp("test", cfg, newDitherer(ditherFloydSteinberg))
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("clamp refresh: %v", err)
	}
	if c.idx != c.totalFrames-1 {
		t.Fatalf("clamped to frame %d, want last frame %d", c.idx, c.totalFrames-1)
	}
}
