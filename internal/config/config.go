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
	// Keep controls how long per-request directories in which codex ran are
	// preserved after the response is sent: forever when unset.
	Keep *Keep `yaml:"keep"`
}

// Keep is workspace.keep: false or 0 removes a workdir right after its request,
// true keeps workdirs forever, and N >= 1 keeps them N days (day directories
// dated before today minus N days are pruned every midnight).
type Keep struct {
	// days is the retention in days; keepForever means no pruning.
	days int
}

const keepForever = -1

// KeepDays returns a Keep that preserves workdirs for n days (0 = not at all).
func KeepDays(n int) *Keep { return &Keep{days: n} }

// KeepForever returns a Keep that never removes workdirs.
func KeepForever() *Keep { return &Keep{days: keepForever} }

func (k *Keep) UnmarshalYAML(node *yaml.Node) error {
	var err error
	switch node.ShortTag() {
	case "!!bool":
		var b bool
		if err = node.Decode(&b); err == nil {
			k.days = 0
			if b {
				k.days = keepForever
			}
			return nil
		}
	case "!!int":
		var n int
		if err = node.Decode(&n); err == nil && n >= 0 {
			k.days = n
			return nil
		}
	}
	return fmt.Errorf("workspace.keep must be true, false or a number of days >= 0, got %q", node.Value)
}

func (k *Keep) String() string {
	switch {
	case k == nil || k.days == keepForever:
		return "forever"
	case k.days == 0:
		return "none"
	default:
		return fmt.Sprintf("%dd", k.days)
	}
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

// KeepWorkdirs reports whether per-request directories are kept after their
// request (default true).
func (w Workspace) KeepWorkdirs() bool {
	return w.Keep == nil || w.Keep.days != 0
}

// RetentionDays returns N when workdirs are kept for N >= 1 days, and 0 when
// no periodic pruning is needed (kept forever, or removed per request).
func (w Workspace) RetentionDays() int {
	if w.Keep == nil || w.Keep.days == keepForever {
		return 0
	}
	return w.Keep.days
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
	// The built-in image_gen tool accepts at most 5 referenced images.
	if c.Limits.MaxImages <= 0 || c.Limits.MaxImages > 5 {
		c.Limits.MaxImages = 5
	}
	if c.Limits.MaxN <= 0 || c.Limits.MaxN > 10 {
		c.Limits.MaxN = 10
	}
	return nil
}
