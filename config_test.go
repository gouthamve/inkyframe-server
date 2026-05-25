package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes content to a temp config file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

const minimalImmich = `
apps:
  - name: photos
    type: immich
    immich:
      url: https://immich.example.com
      api_key: key
      album_id: album
`

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, minimalImmich))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr default: got %q, want :8080", cfg.ListenAddr)
	}
	if !cfg.Rotate {
		t.Error("Rotate default: got false, want true")
	}
	if cfg.Dither != ditherFloydSteinberg {
		t.Errorf("Dither default: got %d, want floyd-steinberg (%d)", cfg.Dither, ditherFloydSteinberg)
	}
	if len(cfg.Apps) != 1 {
		t.Fatalf("got %d apps, want 1", len(cfg.Apps))
	}
}

func TestLoadConfigDitherAtkinson(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "dither: atkinson\n"+minimalImmich))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Dither != ditherAtkinson {
		t.Errorf("Dither: got %d, want atkinson (%d)", cfg.Dither, ditherAtkinson)
	}
}

func TestLoadConfigParsesValues(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `
listen_addr: ":9000"
rotate: true
apps:
  - name: photos
    type: immich
    immich:
      url: https://immich.example.com
      api_key: key
      album_id: album
`))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ListenAddr != ":9000" {
		t.Errorf("ListenAddr: got %q, want :9000", cfg.ListenAddr)
	}
	if !cfg.Rotate {
		t.Error("Rotate: got false, want true")
	}
	if cfg.Apps[0].Immich.URL != "https://immich.example.com" {
		t.Errorf("immich url: got %q", cfg.Apps[0].Immich.URL)
	}
}

func TestLoadConfigExplicitRotateFalse(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "rotate: false\n"+minimalImmich))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Rotate {
		t.Error("Rotate: explicit 'rotate: false' should override the default")
	}
}

func TestLoadConfigErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string // substring expected in the error
	}{
		{
			name: "no apps",
			yaml: "rotate: true\n",
			want: "at least one app",
		},
		{
			name: "missing immich block",
			yaml: "apps:\n  - name: p\n    type: immich\n",
			want: "missing immich config block",
		},
		{
			name: "missing immich fields",
			yaml: "apps:\n  - name: p\n    type: immich\n    immich:\n      url: https://x\n",
			want: "requires url, api_key, and album_id",
		},
		{
			name: "missing movie block",
			yaml: "apps:\n  - name: m\n    type: movie\n",
			want: "missing movie config block",
		},
		{
			name: "missing movie path",
			yaml: "apps:\n  - name: m\n    type: movie\n    movie:\n      fps: 1\n",
			want: "movie requires path",
		},
		{
			name: "negative movie fps",
			yaml: "apps:\n  - name: m\n    type: movie\n    movie:\n      path: /x.mp4\n      fps: -1\n",
			want: "movie fps must be >= 0",
		},
		{
			name: "unknown type",
			yaml: "apps:\n  - name: p\n    type: weather\n",
			want: `unknown type "weather"`,
		},
		{
			name: "missing type",
			yaml: "apps:\n  - name: p\n",
			want: "type is required",
		},
		{
			name: "duplicate names",
			yaml: `
apps:
  - name: dup
    type: immich
    immich: {url: u, api_key: k, album_id: a}
  - name: dup
    type: immich
    immich: {url: u, api_key: k, album_id: a}
`,
			want: `duplicate app name "dup"`,
		},
		{
			name: "unknown dither algorithm",
			yaml: "dither: ordered\n" + minimalImmich,
			want: `unknown dither "ordered"`,
		},
		{
			name: "unknown top-level key",
			yaml: "bogus_key: 1\n" + minimalImmich,
			want: "field bogus_key not found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(writeConfig(t, tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLoadConfigMovie(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `
apps:
  - name: clip
    type: movie
    movie:
      path: /movies/clip.mp4
      fps: 2
      state_file: /state/clip.idx
`))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	mc := cfg.Apps[0].Movie
	if mc == nil {
		t.Fatal("movie config block is nil")
	}
	if mc.Path != "/movies/clip.mp4" || mc.FPS != 2 || mc.StateFile != "/state/clip.idx" {
		t.Errorf("movie config: got %+v", *mc)
	}
	// buildApps wires the movie app without touching ffmpeg (probing happens in
	// Refresh), so it should succeed even without the file present.
	apps, err := buildApps(cfg)
	if err != nil {
		t.Fatalf("buildApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name() != "clip" {
		t.Fatalf("got %d apps (first %q), want 1 named clip", len(apps), apps[0].Name())
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing config file, got nil")
	}
}

func TestBuildApps(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, minimalImmich))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	apps, err := buildApps(cfg)
	if err != nil {
		t.Fatalf("buildApps: %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("got %d apps, want 1", len(apps))
	}
	if apps[0].Name() != "photos" {
		t.Errorf("app name: got %q, want photos", apps[0].Name())
	}
}
