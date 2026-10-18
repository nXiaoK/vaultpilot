package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewRestrictsExistingStateAndCacheDirectories(t *testing.T) {
	for _, separateCache := range []bool{false, true} {
		name := "default_cache"
		if separateCache {
			name = "separate_cache"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			state := filepath.Join(base, "state")
			cache := filepath.Join(state, "cache")
			config := Config{StateDir: state}
			if separateCache {
				cache = filepath.Join(base, "cache")
				config.CacheDir = cache
			}
			for _, dir := range []string{state, cache} {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			r, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { r.Close() })
			for _, dir := range []string{state, cache} {
				info, err := os.Stat(dir)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0700 {
					t.Fatalf("runtime directory retains broad permissions: %s %o", dir, info.Mode().Perm())
				}
			}
		})
	}
}
