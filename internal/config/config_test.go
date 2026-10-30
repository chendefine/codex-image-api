package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("workspace:\n  dir: ws\ncodex:\n  timeout: 2m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.Workspace.Dir) || !cfg.Workspace.KeepWorkdirs() {
		t.Errorf("workspace = %+v", cfg.Workspace)
	}
	if cfg.Server.Listen != ":8080" || cfg.Codex.Bin != "codex" || cfg.Codex.Sandbox != "workspace-write" {
		t.Errorf("defaults not applied: %+v %+v", cfg.Server, cfg.Codex)
	}
	if cfg.Codex.Timeout != 2*time.Minute || cfg.Codex.MaxConcurrency != 2 {
		t.Errorf("codex = %+v", cfg.Codex)
	}
	if cfg.Limits.MaxImageBytes != 50<<20 || cfg.Limits.MaxImages != 5 || cfg.Limits.MaxN != 10 {
		t.Errorf("limits = %+v", cfg.Limits)
	}
}

func TestLoadRequiresWorkspace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("server:\n  listen: :1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for missing workspace.dir")
	}
}

func TestLoadKeep(t *testing.T) {
	for _, tc := range []struct {
		yaml      string
		keep      bool
		retention int
		str       string
	}{
		{"", true, 0, "forever"},
		{"keep: true", true, 0, "forever"},
		{"keep: false", false, 0, "none"},
		{"keep: 0", false, 0, "none"},
		{"keep: 3", true, 3, "3d"},
	} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte("workspace:\n  dir: ws\n  "+tc.yaml+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("%q: %v", tc.yaml, err)
		}
		ws := cfg.Workspace
		if ws.KeepWorkdirs() != tc.keep || ws.RetentionDays() != tc.retention || ws.Keep.String() != tc.str {
			t.Errorf("%q: keep=%v retention=%d str=%s", tc.yaml, ws.KeepWorkdirs(), ws.RetentionDays(), ws.Keep.String())
		}
	}
	for _, bad := range []string{"keep: -1", "keep: forever", "keep: 1.5"} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte("workspace:\n  dir: ws\n  "+bad+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestLoadTokenRefresh(t *testing.T) {
	for _, tc := range []struct {
		yaml      string
		enabled   bool
		at        string
		threshold time.Duration
	}{
		{"", true, "00:00:00", 25 * time.Hour},
		{"token_refresh:\n    enabled: false", false, "00:00:00", 25 * time.Hour},
		{"token_refresh:\n    at: \"03:30\"\n    threshold: 48h", true, "03:30:00", 48 * time.Hour},
		{"token_refresh:\n    at: 23:59:30", true, "23:59:30", 25 * time.Hour},
	} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte("workspace:\n  dir: ws\ncodex:\n  "+tc.yaml+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("%q: %v", tc.yaml, err)
		}
		tr := cfg.Codex.TokenRefresh
		if tr.IsEnabled() != tc.enabled || tr.At.String() != tc.at || tr.Threshold != tc.threshold {
			t.Errorf("%q: enabled=%v at=%s threshold=%s", tc.yaml, tr.IsEnabled(), tr.At, tr.Threshold)
		}
	}
	for _, bad := range []string{"at: 24:00", "at: noon", "at: 7"} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte("workspace:\n  dir: ws\ncodex:\n  token_refresh:\n    "+bad+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestClockTimeNext(t *testing.T) {
	loc := time.FixedZone("X", 8*3600)
	c := Clock(3, 0, 0)
	for _, tc := range []struct{ now, want time.Time }{
		{time.Date(2026, 10, 30, 2, 59, 59, 0, loc), time.Date(2026, 10, 30, 3, 0, 0, 0, loc)},
		{time.Date(2026, 10, 30, 3, 0, 0, 0, loc), time.Date(2026, 10, 31, 3, 0, 0, 0, loc)},
		{time.Date(2026, 12, 31, 23, 0, 0, 0, loc), time.Date(2027, 1, 1, 3, 0, 0, 0, loc)},
	} {
		if got := c.Next(tc.now); !got.Equal(tc.want) {
			t.Errorf("Next(%s) = %s, want %s", tc.now, got, tc.want)
		}
	}
}
