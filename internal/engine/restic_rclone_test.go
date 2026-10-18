package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise Restic's real CSV flag parser and its Rclone subprocess transport,
// using Rclone's local backend so this test needs no cloud account or credentials.
func TestRealResticRcloneTransport(t *testing.T) {
	rclone := os.Getenv("VP_TEST_RCLONE")
	if rclone == "" {
		t.Skip("VP_TEST_RCLONE is required for the real Rclone transport test")
	}
	rclone, err := filepath.Abs(rclone)
	if err != nil {
		t.Fatal(err)
	}
	shellQuote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	program := filepath.Join(t.TempDir(), "rclone")
	for _, name := range []string{"ordinary-cache", "cache with spaces, \"quotes\", 'apostrophe', \\backslash\nand-newline"} {
		t.Run(name, func(t *testing.T) {
			s, plan, _, account := realFixture(t)
			cache := filepath.Join(t.TempDir(), name)
			if err := os.Mkdir(cache, 0700); err != nil {
				t.Fatal(err)
			}
			s.rt.Config.CacheDir = cache
			record := filepath.Join(t.TempDir(), "config-path")
			// Check the inherited config, then forward every operation to the actual engine.
			wrapper := "#!/bin/sh\n" +
				"if [ ! -f \"$RCLONE_CONFIG\" ]; then echo 'missing task config' >&2; exit 1; fi\n" +
				"printf '%s' \"$RCLONE_CONFIG\" > " + shellQuote(record) + "\n" +
				"exec " + shellQuote(rclone) + " \"$@\"\n"
			if err := os.WriteFile(program, []byte(wrapper), 0700); err != nil {
				t.Fatal(err)
			}
			s.rt.Config.RcloneBin = program
			sealed, err := s.rt.Keys.Seal([]byte("disposable-transport-test-password"))
			if err != nil {
				t.Fatal(err)
			}
			repo := Repository{ID: uid(), Path: filepath.Join(account.LocalPath, "transport-repo"), Password: base64.StdEncoding.EncodeToString(sealed)}
			// :local is an on-the-fly Rclone remote, rather than the direct Restic local backend.
			account.Backend, account.Remote = "onedrive", ":local"
			task := Task{ID: uid()}
			ctx := context.Background()
			if output, err := s.restic(ctx, &task, repo, account, []string{"init"}, nil); err != nil {
				t.Fatalf("init through Rclone: %v\n%s", err, output)
			}
			var source Source
			if err := s.rt.Store.Get(ctx, "sources", plan.SourceIDs[0], &source); err != nil {
				t.Fatal(err)
			}
			if output, err := s.restic(ctx, &task, repo, account, []string{"backup", source.Path}, nil); err != nil {
				t.Fatalf("backup through Rclone: %v\n%s", err, output)
			}
			output, err := s.restic(ctx, &task, repo, account, []string{"snapshots"}, nil)
			if err != nil {
				t.Fatalf("snapshots through Rclone: %v\n%s", err, output)
			}
			var snapshots []struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(output, &snapshots); err != nil || len(snapshots) != 1 {
				t.Fatalf("expected one real snapshot: %v\n%s", err, output)
			}
			if output, err := s.restic(ctx, &task, repo, account, []string{"check", "--read-data"}, nil); err != nil {
				t.Fatalf("full check through Rclone: %v\n%s", err, output)
			}
			inherited, err := os.ReadFile(record)
			if err != nil || !strings.HasPrefix(string(inherited), cache+string(os.PathSeparator)+"rclone-") {
				t.Fatalf("Rclone did not receive the task config path: %q %v", inherited, err)
			}
			if _, err := os.Stat(string(inherited)); !os.IsNotExist(err) {
				t.Fatalf("temporary Rclone configuration was not released: %v", err)
			}
			files, err := os.ReadDir(cache)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if strings.HasPrefix(file.Name(), "rclone-") || strings.HasPrefix(file.Name(), "repository-secret-") {
					t.Fatalf("temporary credentials were not released: %s", file.Name())
				}
			}
		})
	}
}
