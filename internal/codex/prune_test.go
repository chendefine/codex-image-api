package codex

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/chendefine/codex-image-api/internal/config"
)

func TestPruneWorkdirs(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 5, 0, time.Local)
	for _, tc := range []struct {
		keep *config.Keep
		want []string
	}{
		{config.KeepDays(1), []string{"20261010", "20261011", "file", "notadate"}},
		{config.KeepDays(2), []string{"20261009", "20261010", "20261011", "file", "notadate"}},
		{config.KeepForever(), []string{"20261008", "20261009", "20261010", "20261011", "file", "notadate"}},
	} {
		dir := t.TempDir()
		for _, d := range []string{"20261008/a", "20261009/b", "20261010", "20261011", "notadate"} {
			if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{Workspace: config.Workspace{Dir: dir, Keep: tc.keep}, Codex: config.Codex{MaxConcurrency: 1}}
		NewRunner(cfg, nil).PruneWorkdirs(now)
		entries, _ := os.ReadDir(dir)
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("keep %s: left %v, want %v", tc.keep, got, tc.want)
		}
	}
}
