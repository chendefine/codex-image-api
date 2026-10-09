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
	if cfg.Limits.MaxImageBytes != 50<<20 || cfg.Limits.MaxImages != 16 || cfg.Limits.MaxN != 10 {
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
