package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"bsc/config"
	"bsc/store"
)

// snapshotPrefix names the files this writes, and is also how pruning
// recognises its own output rather than deleting something else in the
// directory.
const snapshotPrefix = "bsc-"

// runSnapshots periodically writes a consistent copy of the whole database.
//
// This is the backup story, and it is one call: bbolt can write a coherent
// snapshot from inside a read transaction, with no external tooling and no
// coordination with the writer. It is also the only way to inspect the data
// while the service runs, since the live file is exclusively locked.
//
// Snapshots are written in the clear. The database is not the crown jewel —
// the master secret never enters it, and anyone who can read the file is
// already on the box where that secret lives — so encryption belongs to
// whatever ships these off the machine, not here.
func runSnapshots(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	if cfg.SnapshotDir == "" {
		log.Info("snapshots disabled", "reason", "snapshot.dir is unset")
		return nil
	}
	if err := os.MkdirAll(cfg.SnapshotDir, 0o700); err != nil {
		return fmt.Errorf("snapshots: %w", err)
	}
	log = log.With("dir", cfg.SnapshotDir)
	t := time.NewTicker(cfg.SnapshotInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		// A failed snapshot must never stop the service: losing a backup is bad,
		// stopping the chain watcher because of it is worse.
		if err := snapshot(st, cfg.SnapshotDir, cfg.SnapshotKeep, log); err != nil {
			log.Error("snapshot failed", "err", err)
		}
	}
}

// snapshot writes one copy and prunes the oldest beyond keep.
func snapshot(st *store.Store, dir string, keep int, log *slog.Logger) error {
	name := filepath.Join(dir, snapshotPrefix+time.Now().UTC().Format("20060102T150405Z")+".db")

	// Write to a temporary name and rename into place, so a crash mid-write
	// cannot leave a truncated file that looks like a usable backup.
	tmp := name + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := st.Snapshot(f)
	if err != nil {
		f.Close()      //nolint:errcheck
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		return err
	}
	log.Info("snapshot written", "file", filepath.Base(name), "bytes", n)
	return prune(dir, keep, log)
}

// prune keeps the newest `keep` snapshots. Names sort chronologically, which is
// why the timestamp format is what it is.
func prune(dir string, keep int, log *slog.Logger) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".db" &&
			len(e.Name()) > len(snapshotPrefix) && e.Name()[:len(snapshotPrefix)] == snapshotPrefix {
			names = append(names, e.Name())
		}
	}
	if len(names) <= keep {
		return nil
	}
	sort.Strings(names)
	for _, old := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(dir, old)); err != nil {
			return err
		}
		log.Info("snapshot pruned", "file", old)
	}
	return nil
}
