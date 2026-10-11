package platform

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// cleanupStaleManifestDirs runs only during startup, before engines can use
// the cache. Manifest staging trees contain directory placeholders, not source
// data, and no task can reuse them after a restart. Do not run this from cache
// budget checks: another backup may still be using its staging tree.
func cleanupStaleManifestDirs(cacheDir string) error {
	cache, err := os.OpenRoot(cacheDir)
	if err != nil {
		return err
	}
	defer cache.Close()
	dir, err := cache.Open(".")
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		// MkdirTemp uses a decimal suffix. Ignore unrelated names and entry
		// symlinks, even if their names resemble a manifest staging tree.
		name := entry.Name()
		suffix, matched := strings.CutPrefix(name, "manifest-dirs-")
		if !matched || suffix == "" || strings.IndexFunc(suffix, func(r rune) bool { return r < '0' || r > '9' }) >= 0 || !entry.IsDir() {
			continue
		}
		if err := removeStaleManifestDir(cache, name); err != nil {
			return fmt.Errorf("无法清理遗留备份临时目录 %s：%w；请停止服务后由目录所有者或管理员清理该临时目录", filepath.Join(cacheDir, name), err)
		}
	}
	return nil
}

func removeStaleManifestDir(cache *os.Root, name string) error {
	// Recheck through the root before changing permissions. Root confines all
	// access to the cache; WalkDir never descends through symlinks.
	info, err := cache.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("临时目录入口已改变")
	}
	if err := cache.Chmod(name, 0700); err != nil {
		return err
	}
	stage, err := cache.OpenRoot(name)
	if err != nil {
		return err
	}
	defer stage.Close()
	if err := fs.WalkDir(stage.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			// This runs before WalkDir reads each directory, so even 0000
			// ancestors can be repaired by the original process owner.
			return stage.Chmod(path, 0700)
		}
		return nil
	}); err != nil {
		return err
	}
	return cache.RemoveAll(name)
}
