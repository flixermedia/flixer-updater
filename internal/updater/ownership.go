package updater

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

const targetUIDGID = 1000

// DefaultDataPaths are the data directories that moved from legacy container UIDs to 1000:1000.
var DefaultDataPaths = []string{"/data/cache", "/data/ai"}

// NormalizeOwnership ensures every non-symlink entry under each existing directory is owned by 1000:1000.
// Errors on individual entries are logged and ignored so a single unreadable path never blocks updates.
func NormalizeOwnership(log *slog.Logger, dirs ...string) int {
	if log == nil {
		log = slog.Default()
	}
	changed := 0
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				log.Warn("ownership check skipped path", "path", path, "error", walkErr)
				return nil
			}
			info, err := d.Info()
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				return nil
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return nil
			}
			if int(stat.Uid) == targetUIDGID && int(stat.Gid) == targetUIDGID {
				return nil
			}
			if err := os.Chown(path, targetUIDGID, targetUIDGID); err != nil {
				log.Warn("failed to update path ownership", "path", path, "error", err)
				return nil
			}
			changed++
			return nil
		})
	}
	if changed > 0 {
		log.Info("normalized data directory ownership to 1000:1000", "updated_entries", changed)
	}
	return changed
}
