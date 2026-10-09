package codex

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// RunPruner enforces workspace.keep = N days until ctx is done: it prunes
// immediately (catching up on midnights missed while stopped) and then every
// local midnight. It returns immediately when no retention is configured.
func (r *Runner) RunPruner(ctx context.Context) {
	if r.ws.RetentionDays() == 0 {
		return
	}
	for {
		now := time.Now()
		r.PruneWorkdirs(now)
		y, m, d := now.Date()
		timer := time.NewTimer(time.Until(time.Date(y, m, d+1, 0, 0, 0, 0, now.Location())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// PruneWorkdirs removes the <YYYYMMDD> day directories of the workspace dated
// before now's day minus workspace.keep days. Other entries are left alone.
func (r *Runner) PruneWorkdirs(now time.Time) {
	days := r.ws.RetentionDays()
	if days == 0 {
		return
	}
	y, m, d := now.Date()
	cutoff := time.Date(y, m, d-days, 0, 0, 0, 0, now.Location())
	entries, err := os.ReadDir(r.ws.Dir)
	if err != nil {
		r.logf.Warn("prune workspace", "dir", r.ws.Dir, "err", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) != len("20060102") {
			continue
		}
		day, err := time.ParseInLocation("20060102", e.Name(), now.Location())
		if err != nil || !day.Before(cutoff) {
			continue
		}
		dir := filepath.Join(r.ws.Dir, e.Name())
		if err := os.RemoveAll(dir); err != nil {
			r.logf.Warn("prune workdirs", "dir", dir, "err", err)
			continue
		}
		r.logf.Info("pruned workdirs", "dir", dir, "keep_days", days)
	}
}
