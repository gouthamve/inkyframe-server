package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// rawFrameSize is the byte length of one ffmpeg rgb24 frame at the canvas size.
	rawFrameSize = canvasWidth * canvasHeight * 3

	movieProbeTimeout  = 30 * time.Second // ffprobe metadata lookup
	movieDecodeTimeout = 60 * time.Second // one seek+decode of a single frame
)

// movieApp plays a whole video file one frame at a time, decoding on demand:
// each navigation event seeks ffmpeg to the requested frame and decodes just
// that single frame. This keeps memory near-constant regardless of film length
// (a feature film is ~26 GB of framebuffers — far too much to cache), at the
// cost of a sub-second ffmpeg seek per step, which is fine for a frame that
// advances only on a button press.
//
// Only the current frame is cached, so /current-image and /healthz never trigger
// a decode; decoding happens in Refresh / Next / Prev (both /image and
// /next-image drive Next on a movie). The current frame index is persisted to
// statePath (when set) on every step, so a restart resumes where it left off.
//
// Unlike immich it does not embed imageCache: there is no per-serve build to
// regenerate. It implements App directly plus Navigator. navMu serializes
// Next/Prev (so the index advances one frame at a time and the slow ffmpeg
// decode runs without blocking readers), while the short-held mu guards the
// current frame/index for Current/Previous/LastErr.
type movieApp struct {
	name      string
	path      string
	statePath string  // empty disables resume persistence
	fps       float64 // configured sampling fps; 0 means native (every frame)
	dith      *ditherer

	// Set once during Refresh, after probing; immutable afterwards.
	baseCtx     context.Context                                    // server root ctx, bounds decodes
	sfps        float64                                            // effective sampling fps (fps or native)
	totalFrames int                                                // for clamping navigation
	vf          string                                             // ffmpeg scale-to-fill + crop filter
	decode      func(ctx context.Context, idx int) ([]byte, error) // = ffmpegDecodeFrame; swapped in tests

	navMu sync.Mutex // serializes Next/Prev; held across the decode

	mu       sync.Mutex // guards the fields below; held only briefly, never across a decode
	idx      int        // index of the frame currently showing
	cachedFB []byte     // packed 4bpp framebuffer for idx
	loadedAt time.Time  // build time of cachedFB
	lastErr  error
}

func newMovieApp(name string, cfg movieConfig, dith *ditherer) *movieApp {
	a := &movieApp{
		name:      name,
		path:      cfg.Path,
		statePath: cfg.StateFile,
		fps:       cfg.FPS,
		dith:      dith,
	}
	a.decode = a.ffmpegDecodeFrame
	return a
}

// Refresh probes the video, computes the frame mapping, restores the saved
// position, and decodes the starting frame. It runs once at startup (fail-fast:
// a missing ffmpeg/ffprobe, missing file, or undecodable video surfaces
// immediately) and records build metrics like the other apps.
func (a *movieApp) Refresh(ctx context.Context) error {
	start := time.Now()
	err := a.load(ctx)
	recordBuild(a.name, "refresh", err, time.Since(start))

	a.mu.Lock()
	a.lastErr = err
	a.mu.Unlock()
	return err
}

// load does the work of Refresh: probe, compute sfps/totalFrames/vf, restore the
// persisted index (clamped), and decode that frame into the cache.
func (a *movieApp) load(ctx context.Context) error {
	a.baseCtx = ctx

	nativeFPS, duration, nbFrames, err := a.probe(ctx)
	if err != nil {
		return err
	}

	sfps := a.fps
	if sfps <= 0 {
		sfps = nativeFPS // native: index over every frame
	}
	if sfps <= 0 {
		return fmt.Errorf("movie %q: could not determine a frame rate", a.name)
	}

	// Prefer the container's exact frame count when sampling at native rate;
	// otherwise derive the count from duration × sampling fps. Round up so the
	// final partial interval stays navigable rather than being truncated away
	// (overshooting by one is harmless — that index just fails to decode and is
	// clamped, see step).
	total := nbFrames
	if a.fps > 0 || total <= 0 {
		total = int(math.Ceil(duration * sfps))
	}
	if total < 1 {
		total = 1
	}

	a.mu.Lock()
	a.sfps = sfps
	a.totalFrames = total
	a.vf = fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d",
		canvasWidth, canvasHeight, canvasWidth, canvasHeight)
	idx := a.readState()
	if idx > total-1 {
		idx = total - 1
	}
	a.idx = idx
	a.mu.Unlock()

	fb, err := a.decode(ctx, idx)
	if err != nil && idx != 0 {
		// A stale/edge resume index that won't decode shouldn't make startup
		// fatal when the file itself is fine — fall back to the first frame.
		log.Printf("movie %q: could not decode resume frame %d (%v); starting from the beginning", a.name, idx, err)
		idx = 0
		fb, err = a.decode(ctx, idx)
	}
	if err != nil {
		return fmt.Errorf("decode initial frame %d: %w", idx, err)
	}

	a.mu.Lock()
	a.idx = idx
	a.cachedFB = fb
	a.loadedAt = time.Now()
	a.mu.Unlock()

	log.Printf("movie %q: %d frames @ %.3f fps from %s; resuming at frame %d", a.name, total, sfps, a.path, idx)
	return nil
}

// Regenerate is a no-op: frames are decoded on demand by Next/Prev, not in the
// background.
func (a *movieApp) Regenerate(context.Context) {}

func (a *movieApp) Name() string { return a.name }

func (a *movieApp) Current() ([]byte, time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cachedFB, a.loadedAt
}

// Previous returns the current cached frame. Movie navigation goes through
// Next/Prev (Navigator), so the manager doesn't call this for /prev-image; it's
// here to satisfy the App interface and /healthz without forcing a decode.
func (a *movieApp) Previous() ([]byte, time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cachedFB, a.loadedAt
}

func (a *movieApp) LastErr() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastErr
}

// Next advances one frame, clamping at the last, and returns the frame showing.
func (a *movieApp) Next() ([]byte, time.Time) { return a.step(+1) }

// Prev steps back one frame, clamping at the first, and returns the frame showing.
func (a *movieApp) Prev() ([]byte, time.Time) { return a.step(-1) }

// step moves the frame index by delta (±1), clamping at the ends, decodes the
// new frame, and on success makes it current and persists the position. The slow
// work — the ffmpeg decode and the state-file write — runs WITHOUT a.mu held, so
// it never blocks readers (/image, /current-image, /healthz); a.mu is taken only
// briefly to snapshot the current state and to commit the result. navMu
// serializes concurrent steps so the index advances one frame at a time. On a
// decode failure the current frame is kept (and the error recorded) so a
// transient ffmpeg hiccup never blanks the display.
func (a *movieApp) step(delta int) ([]byte, time.Time) {
	a.navMu.Lock()
	defer a.navMu.Unlock()

	a.mu.Lock()
	cur, total := a.idx, a.totalFrames
	cfb, cat := a.cachedFB, a.loadedAt
	a.mu.Unlock()

	newIdx := cur + delta
	if newIdx < 0 || newIdx >= total {
		return cfb, cat // clamped at an end: nothing to decode
	}

	start := time.Now()
	fb, err := a.decode(a.baseCtx, newIdx)
	recordBuild(a.name, "navigate", err, time.Since(start))
	if err != nil {
		a.mu.Lock()
		a.lastErr = err
		a.mu.Unlock()
		log.Printf("movie %q: decode frame %d failed (keeping frame %d): %v", a.name, newIdx, cur, err)
		return cfb, cat
	}

	now := time.Now()
	a.mu.Lock()
	a.idx = newIdx
	a.cachedFB = fb
	a.loadedAt = now
	a.lastErr = nil
	a.mu.Unlock()

	a.persist(newIdx) // file I/O; outside a.mu, navMu keeps writes ordered
	return fb, now
}

// ffmpegDecodeFrame seeks to frame idx and decodes that single frame to a packed
// 4bpp framebuffer. ffmpeg does the decode + a high-quality scale-to-fill +
// centre-crop to the exact canvas size; we only dither + pack. -ss before -i is
// a fast (keyframe) seek.
//
// We seek to (idx-0.5)/sfps, half a frame before the frame's nominal
// presentation time, not idx/sfps. Accurate seeking returns the first frame
// whose PTS >= the seek time; aiming at the exact PTS is fragile to float
// rounding (a hair high silently returns the next frame, or nothing past the
// last frame), so we land squarely inside the gap before frame idx instead.
func (a *movieApp) ffmpegDecodeFrame(ctx context.Context, idx int) ([]byte, error) {
	t := (float64(idx) - 0.5) / a.sfps
	if t < 0 {
		t = 0
	}

	cctx, cancel := context.WithTimeout(ctx, movieDecodeTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-ss", strconv.FormatFloat(t, 'f', 6, 64),
		"-i", a.path,
		"-vf", a.vf,
		"-frames:v", "1",
		"-pix_fmt", "rgb24",
		"-f", "rawvideo",
		"-an",
		"pipe:1",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg (is it installed and on PATH?): %w", err)
	}

	fb, decErr := readFrame(stdout, a.dith)
	// Drain anything left so ffmpeg never blocks on a full pipe, then reap it.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

	if waitErr != nil {
		return nil, fmt.Errorf("ffmpeg failed: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	if decErr != nil {
		if errors.Is(decErr, io.EOF) || errors.Is(decErr, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("no frame decoded at index %d (t=%.3fs); past end of video?", idx, t)
		}
		return nil, fmt.Errorf("read frame: %w", decErr)
	}
	return fb, nil
}

// probe runs ffprobe to read the video's native frame rate, duration, and (when
// the container records it) exact frame count.
func (a *movieApp) probe(ctx context.Context) (fps, duration float64, nbFrames int, err error) {
	cctx, cancel := context.WithTimeout(ctx, movieProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=r_frame_rate,nb_frames",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1",
		a.path,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("ffprobe %s (is ffprobe installed and on PATH?): %w: %s", a.path, err, strings.TrimSpace(stderr.String()))
	}

	fields := parseProbeFields(out)
	fps, err = parseFrameRate(fields["r_frame_rate"])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse frame rate %q: %w", fields["r_frame_rate"], err)
	}
	duration, _ = strconv.ParseFloat(strings.TrimSpace(fields["duration"]), 64)
	if n, e := strconv.Atoi(strings.TrimSpace(fields["nb_frames"])); e == nil {
		nbFrames = n
	}
	return fps, duration, nbFrames, nil
}

// parseProbeFields turns ffprobe's "key=value" line output into a map.
func parseProbeFields(out []byte) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

// parseFrameRate parses an ffprobe frame rate, which is usually a rational like
// "24000/1001" but may be a plain number.
func parseFrameRate(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty frame rate")
	}
	if num, den, ok := strings.Cut(s, "/"); ok {
		n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
		if err != nil {
			return 0, err
		}
		d, err := strconv.ParseFloat(strings.TrimSpace(den), 64)
		if err != nil {
			return 0, err
		}
		if d == 0 {
			return 0, fmt.Errorf("zero denominator in %q", s)
		}
		return n / d, nil
	}
	return strconv.ParseFloat(s, 64)
}

// readFrame reads exactly one rgb24 frame of canvasWidth×canvasHeight from r,
// then dithers and packs it into a 4bpp framebuffer. Keeping it independent of
// ffmpeg lets it be unit-tested with a synthetic byte stream. A short read
// (io.EOF / io.ErrUnexpectedEOF) is returned to the caller to interpret.
func readFrame(r io.Reader, dz *ditherer) ([]byte, error) {
	buf := make([]byte, rawFrameSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}

	img := image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))
	dst := img.Pix
	for i, n := 0, canvasWidth*canvasHeight; i < n; i++ {
		dst[i*4+0] = buf[i*3+0]
		dst[i*4+1] = buf[i*3+1]
		dst[i*4+2] = buf[i*3+2]
		dst[i*4+3] = 0xFF
	}
	return packFramebuffer(dz.ditherImage(img)), nil
}

// readState returns the persisted frame index, or 0 if persistence is disabled,
// the file is absent, or its contents don't parse. The result is clamped to the
// valid range by the caller (it doesn't yet know totalFrames here).
func (a *movieApp) readState() int {
	if a.statePath == "" {
		return 0
	}
	data, err := os.ReadFile(a.statePath)
	if err != nil {
		return 0 // first run or unreadable: start at the beginning
	}
	idx, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || idx < 0 {
		return 0
	}
	return idx
}

// persist atomically writes the current frame index to statePath (temp file in
// the same directory + rename). It's a no-op when persistence is disabled, and
// logs rather than fails on error so a serve is never broken by a write hiccup.
func (a *movieApp) persist(idx int) {
	if a.statePath == "" {
		return
	}
	dir := filepath.Dir(a.statePath)
	tmp, err := os.CreateTemp(dir, ".inkystate-*")
	if err != nil {
		log.Printf("movie %q: cannot write state file: %v", a.name, err)
		return
	}
	name := tmp.Name()
	_, werr := tmp.WriteString(strconv.Itoa(idx))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(name)
		log.Printf("movie %q: cannot write state file: %v", a.name, errors.Join(werr, cerr))
		return
	}
	if err := os.Rename(name, a.statePath); err != nil {
		_ = os.Remove(name)
		log.Printf("movie %q: cannot persist state to %s: %v", a.name, a.statePath, err)
	}
}
