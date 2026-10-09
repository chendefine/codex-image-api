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
