package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRcloneHelpersIgnoreUnrelatedBrokenAccount(t *testing.T) {
	s := catalogRefreshNode(t)
	good := Account{ID: "local-account", Name: "本地账号", Backend: "local", Remote: ":local", Verified: true, Enabled: true}
	bad := Account{ID: "broken-account", Name: "损坏账号", Backend: "onedrive", Remote: "vp_broken", Verified: true, Enabled: true}
	pool := Pool{ID: "pool", AccountIDs: []string{good.ID, bad.ID}}
	ctx := context.Background()
	for _, a := range []Account{good, bad} {
		if _, err := s.rt.Store.Save(ctx, "accounts", a.ID, 0, a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.rt.Store.Save(ctx, "storage_pools", pool.ID, 0, pool); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(t.TempDir(), "rclone")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n[ \"$1\" = --config ] && [ -f \"$2\" ] || exit 1\nprintf '%s\\n' \"$@\"\ncat \"$2\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.rt.Config.RcloneBin = program
	calls := map[string]func() ([]byte, error){
		"output":  func() ([]byte, error) { return s.rcloneOutput(ctx, []Account{good}, "lsf", ":local:.") },
		"run":     func() ([]byte, error) { return s.rcloneRun(ctx, []Account{good}, nil, "lsf", ":local:.") },
		"bounded": func() ([]byte, error) { return s.rcloneOutputBounded(ctx, []Account{good}, 4096, "lsf", ":local:.") },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			out, err := call()
			if err != nil || !strings.Contains(string(out), "lsf") || strings.Contains(string(out), bad.Remote) {
				t.Fatalf("无关账号阻止调用或泄露配置：%v %s", err, out)
			}
			paths, err := filepath.Glob(filepath.Join(s.rt.Config.CacheDir, "rclone-*.conf"))
			if err != nil || len(paths) != 0 {
				t.Fatalf("临时配置未释放：%v %v", paths, err)
			}
		})
	}
	if _, err := s.rcloneOutput(ctx, []Account{bad}, "lsf", bad.Remote+":"); err == nil || !strings.Contains(err.Error(), bad.Name) {
		t.Fatalf("目标账号错误未保留：%v", err)
	}
}
