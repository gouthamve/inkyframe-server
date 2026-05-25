package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// config is the validated server configuration loaded from the YAML file.
type config struct {
	ListenAddr string
	Rotate     bool
	Dither     ditherAlgo
	Apps       []appConfig
}

// appConfig declares one app. Type selects the implementation; the matching
// type-specific block (e.g. Immich) carries its settings. The block is a
// pointer so its absence is distinguishable from an empty block.
type appConfig struct {
	Name   string        `yaml:"name"`
	Type   string        `yaml:"type"`
	Immich *immichConfig `yaml:"immich"`
}

// immichConfig is the settings block for an "immich" app.
type immichConfig struct {
	URL     string `yaml:"url"`
	APIKey  string `yaml:"api_key"`
	AlbumID string `yaml:"album_id"`
}

// rawConfig mirrors the on-disk YAML. Rotate is a pointer so an absent key
// (default to true) is distinguishable from an explicit "rotate: false".
type rawConfig struct {
	ListenAddr string      `yaml:"listen_addr"`
	Rotate     *bool       `yaml:"rotate"`
	Dither     string      `yaml:"dither"`
	Apps       []appConfig `yaml:"apps"`
}

// parseDitherAlgo maps the "dither" config value to an algorithm. An empty value
// defaults to Floyd–Steinberg (the prior behaviour).
func parseDitherAlgo(s string) (ditherAlgo, error) {
	switch s {
	case "", "floyd-steinberg":
		return ditherFloydSteinberg, nil
	case "atkinson":
		return ditherAtkinson, nil
	default:
		return 0, fmt.Errorf("unknown dither %q (valid: floyd-steinberg, atkinson)", s)
	}
}

// loadConfig reads, parses, and validates the YAML config file, applying
// defaults. Unknown keys are rejected so typos surface immediately.
func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("read config %s: %w", path, err)
	}

	var raw rawConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return config{}, fmt.Errorf("parse config %s: %w", path, err)
	}

	cfg := config{
		ListenAddr: raw.ListenAddr,
		Rotate:     true, // default on; an explicit "rotate: false" overrides
		Apps:       raw.Apps,
	}
	if raw.Rotate != nil {
		cfg.Rotate = *raw.Rotate
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}

	dither, err := parseDitherAlgo(raw.Dither)
	if err != nil {
		return config{}, err
	}
	cfg.Dither = dither

	if err := validateApps(cfg.Apps); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// validateApps checks that at least one app is declared, names are unique, and
// each app's type-specific block is present and well-formed.
func validateApps(apps []appConfig) error {
	if len(apps) == 0 {
		return errors.New("config must declare at least one app")
	}
	seen := make(map[string]bool, len(apps))
	for i := range apps {
		a := &apps[i]
		if a.Name == "" {
			return fmt.Errorf("apps[%d]: name is required", i)
		}
		if seen[a.Name] {
			return fmt.Errorf("duplicate app name %q", a.Name)
		}
		seen[a.Name] = true

		switch a.Type {
		case "immich":
			ic := a.Immich
			if ic == nil {
				return fmt.Errorf("app %q: missing immich config block", a.Name)
			}
			if ic.URL == "" || ic.APIKey == "" || ic.AlbumID == "" {
				return fmt.Errorf("app %q: immich requires url, api_key, and album_id", a.Name)
			}
		case "":
			return fmt.Errorf("app %q: type is required", a.Name)
		default:
			return fmt.Errorf("app %q: unknown type %q", a.Name, a.Type)
		}
	}
	return nil
}

// buildApps constructs the ordered App list from validated config. apps[0] is
// the app active at startup. New app types are added here, keyed by type.
func buildApps(cfg config) ([]App, error) {
	apps := make([]App, 0, len(cfg.Apps))
	for _, ac := range cfg.Apps {
		switch ac.Type {
		case "immich":
			apps = append(apps, newImmichApp(ac.Name, *ac.Immich, cfg.Dither))
		default:
			return nil, fmt.Errorf("app %q: unsupported type %q", ac.Name, ac.Type)
		}
	}
	return apps, nil
}
