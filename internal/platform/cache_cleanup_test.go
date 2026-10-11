package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStartupRemovesUnreadableStaleManifestDirectories(t *testing.T) {
	base := t.TempDir()
	cache := filepath.Join(base, "cache")
	stale := filepath.Join(cache, "manifest-dirs-3841998261")
	parent := filepath.Join(stale, "repo", "source", ".@#local")
	if err := os.MkdirAll(filepath.Join(parent, "trash", "1Panel", "database"), 0700); err != nil {
		t.Fatal(err)
	}
	// Keep unrelated engine cache intact, including similarly named directories.
	for _, path := range []string{"restic/repository/index", "manifest-dirs-not-a-temp/file", "other/file"} {
		path = filepath.Join(cache, path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(parent, 0000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stale, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(stale, 0700)
		_ = os.Chmod(parent, 0700)
	})
	if os.Geteuid() != 0 {
		r := &Runtime{Config: Config{CacheDir: cache, CacheMaxBytes: 1 << 20}}
		if err := r.CheckCacheBudget(); err == nil {
			t.Fatal("expected pre-fix cache permission failure")
		}
	}
	r, err := New(Config{StateDir: filepath.Join(base, "state"), CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale tree remains: %v", err)
	}
	if err := r.CheckCacheBudget(); err != nil {
		t.Fatal("cache budget remains blocked", err)
	}
	for _, path := range []string{"restic/repository/index", "manifest-dirs-not-a-temp/file", "other/file"} {
		if data, err := os.ReadFile(filepath.Join(cache, path)); err != nil || string(data) != "keep" {
			t.Fatalf("modified unrelated cache %s: %q %v", path, data, err)
		}
	}
	// Admission checks must never remove a running backup's staging tree.
	active := filepath.Join(cache, "manifest-dirs-1234567890")
	if err := os.Mkdir(active, 0700); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckCacheBudget(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatal("budget check removed active manifest tree", err)
	}
	if err := cleanupStaleManifestDirs(cache); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleManifestDirs(cache); err != nil {
		t.Fatal("repeated cleanup failed", err)
	}
}

func TestStaleManifestCleanupDoesNotFollowSymlinks(t *testing.T) {
	base := t.TempDir()
	cache, outside := filepath.Join(base, "cache"), filepath.Join(base, "source")
	for _, path := range []string{cache, outside} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(outside, "business.txt")
	if err := os.WriteFile(file, []byte("business data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0700) })
	entry := filepath.Join(cache, "manifest-dirs-123456789")
	if err := os.Symlink(outside, entry); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(cache, "manifest-dirs-987654321")
	if err := os.Mkdir(stale, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(stale, "external")); err != nil {
		t.Fatal(err)
	}
	// A relative link to a separate cache tree must also remain untouched.
	if err := os.Symlink("../manifest-dirs-123456789", filepath.Join(stale, "relative")); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleManifestDirs(cache); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale tree remains: %v", err)
	}
	if info, err := os.Lstat(entry); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("modified entry symlink: %v %v", info, err)
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("modified source permissions: %v %v", info, err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "business data" {
		t.Fatalf("modified business file: %q %v", data, err)
	}
}
