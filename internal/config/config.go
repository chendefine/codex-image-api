// Package config loads the service configuration from a YAML file.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    Server    `yaml:"server"`
	Workspace Workspace `yaml:"workspace"`
	Codex     Codex     `yaml:"codex"`
	Limits    Limits    `yaml:"limits"`
}

type Server struct {
	Listen  string   `yaml:"listen"`
	APIKeys []string `yaml:"api_keys"`
	// ShutdownTimeout is how long in-flight requests may finish after SIGTERM
	// before they are canceled (which kills their codex processes).
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

type Workspace struct {
	// Dir is the root under which every API call gets its own working directory.
	Dir string `yaml:"dir"`
	// Keep preserves per-request directories in which codex ran after the
	// response is sent.
	Keep *bool `yaml:"keep"`
}

type Codex struct {
	Bin            string            `yaml:"bin"`
	Model          string            `yaml:"model"`
	Profile        string            `yaml:"profile"`
	Sandbox        string            `yaml:"sandbox"`
	CodexHome      string            `yaml:"codex_home"`
	Timeout        time.Duration     `yaml:"timeout"`
	MaxConcurrency int               `yaml:"max_concurrency"`
	ExtraArgs      []string          `yaml:"extra_args"`
	Env            map[string]string `yaml:"env"`
	// MaxQueue is how many requests may wait for a free codex slot; beyond it
	// requests are rejected with 503 at once.
	MaxQueue int `yaml:"max_queue"`
	// QueueTimeout is how long a request may wait for a free codex slot.
	QueueTimeout time.Duration `yaml:"queue_timeout"`
}

type Limits struct {
	MaxImageBytes int64 `yaml:"max_image_bytes"`
	MaxImages     int   `yaml:"max_images"`
	MaxN          int   `yaml:"max_n"`
	// AllowImageURLs lets JSON edit requests reference http(s) image URLs that the
	// server downloads. Disabled by default; data: URLs are always accepted.
	AllowImageURLs bool `yaml:"allow_image_urls"`
}

// KeepWorkdirs reports whether per-request directories are kept (default true).
func (w Workspace) KeepWorkdirs() bool {
	return w.Keep == nil || *w.Keep
}

// Load reads, defaults and validates the configuration at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) normalize() error {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.ShutdownTimeout <= 0 {
		c.Server.ShutdownTimeout = 30 * time.Second
	}
	if c.Workspace.Dir == "" {
		return errors.New("config: workspace.dir is required")
	}
	dir, err := filepath.Abs(c.Workspace.Dir)
	if err != nil {
		return fmt.Errorf("config: workspace.dir: %w", err)
	}
	c.Workspace.Dir = dir

	if c.Codex.Bin == "" {
		c.Codex.Bin = "codex"
	}
	if c.Codex.Sandbox == "" {
		c.Codex.Sandbox = "workspace-write"
	}
	switch c.Codex.Sandbox {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return fmt.Errorf("config: codex.sandbox %q is invalid", c.Codex.Sandbox)
	}
	if c.Codex.CodexHome == "" {
		c.Codex.CodexHome = os.Getenv("CODEX_HOME")
	}
	if c.Codex.CodexHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			c.Codex.CodexHome = filepath.Join(home, ".codex")
		}
	}
	if c.Codex.Timeout <= 0 {
		c.Codex.Timeout = 15 * time.Minute
	}
	if c.Codex.MaxConcurrency <= 0 {
		c.Codex.MaxConcurrency = 2
	}
	if c.Codex.MaxQueue <= 0 {
		c.Codex.MaxQueue = 16
	}
	if c.Codex.QueueTimeout <= 0 {
		c.Codex.QueueTimeout = 5 * time.Minute
	}

	if c.Limits.MaxImageBytes <= 0 {
		c.Limits.MaxImageBytes = 50 << 20
	}
	if c.Limits.MaxImages <= 0 {
		c.Limits.MaxImages = 16
	}
	if c.Limits.MaxN <= 0 || c.Limits.MaxN > 10 {
		c.Limits.MaxN = 10
	}
	return nil
}
