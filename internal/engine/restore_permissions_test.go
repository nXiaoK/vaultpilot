package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"vaultpilot/internal/platform"
	"vaultpilot/internal/store"
)

func permissionRestoreFixture(t *testing.T) (*Service, RestoreRequest) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	restore, cache := filepath.Join(base, "restore"), filepath.Join(base, "cache")
	for _, dir := range []string{restore, cache} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(filepath.Join(base, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	account := Account{ID: "a", Backend: "local", LocalPath: base, Verified: true}
	repo := Repository{ID: "repo", AccountID: account.ID}
	point := Point{ID: "p", RunID: "run", Status: "completed"}
	for table, value := range map[string]any{"accounts": account, "repositories": repo, "recovery_points": point} {
		raw, _ := json.Marshal(value)
		var object map[string]any
		_ = json.Unmarshal(raw, &object)
		if _, err := db.Save(ctx, table, object["id"].(string), 0, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB.Exec("INSERT INTO file_entries(run_id,source_id,path,kind,size,repo_id,snapshot_path) VALUES(?,?,?,?,?,?,?)", "run", "s", "nested/file.txt", "file", 4, "repo", "/file.txt"); err != nil {
		t.Fatal(err)
	}
	s := &Service{rt: &platform.Runtime{Store: db, Config: platform.Config{CacheDir: cache, RestoreRoots: []string{restore}}}}
	return s, RestoreRequest{PointID: point.ID, Target: restore}
}

func requireRestorePermissionError(t *testing.T, err error) {
	t.Helper()
	var f *failure
	if !errors.As(err, &f) || f.Code != "RESTORE_TARGET_NOT_WRITABLE" || !strings.Contains(f.Message, "UID=") || !strings.Contains(f.Message, "VP_RESTORE_HOST") {
		t.Fatalf("expected actionable restore permission error, got %v", err)
	}
}

func TestRestorePreflightChecksExistingAndMissingTargetPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission denial requires a non-root process")
	}
	for _, scenario := range []string{"target", "missing-target", "missing-target-under-locked-parent", "source-parent"} {
		t.Run(scenario, func(t *testing.T) {
			s, req := permissionRestoreFixture(t)
			locked := req.Target
			switch scenario {
			case "missing-target":
				req.Target = filepath.Join(req.Target, "new-target")
			case "missing-target-under-locked-parent":
				locked = filepath.Join(req.Target, "locked-parent")
				req.Target = filepath.Join(locked, "new", "target")
			case "source-parent":
				locked = filepath.Join(req.Target, "nested")
			}
			if err := os.MkdirAll(locked, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(locked, 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(locked, 0700) })
			_, err := s.restorePreview(context.Background(), req)
			requireRestorePermissionError(t, err)
			// The HTTP response must preserve the specific error code for the UI.
			recorder := httptest.NewRecorder()
			problem(recorder, err)
			if !strings.Contains(recorder.Body.String(), "RESTORE_TARGET_NOT_WRITABLE") {
				t.Fatalf("lost preview error code: %s", recorder.Body.String())
			}
			// Execution also preflights before creating directories or invoking Restic.
			raw, _ := json.Marshal(req)
			task := Task{ID: "restore-test", Request: raw}
			requireRestorePermissionError(t, s.restore(context.Background(), &task))
			if scenario != "target" && scenario != "source-parent" {
				if _, err := os.Lstat(req.Target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("preflight created target: %v", err)
				}
			}
		})
	}
}

func TestRestoreWritablePreviewDoesNotCreateTarget(t *testing.T) {
	s, req := permissionRestoreFixture(t)
	req.Target = filepath.Join(req.Target, "new", "target")
	preview, err := s.restorePreview(context.Background(), req)
	if err != nil || preview.Blocked {
		t.Fatalf("writable destination rejected: %+v %v", preview, err)
	}
	entries, err := os.ReadDir(s.rt.Config.RestoreRoots[0])
	if err != nil || len(entries) != 0 {
		t.Fatalf("preview wrote files/directories: %v %v", entries, err)
	}
}

func TestRestoreWritePermissionChangePreservesSpecificError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission denial requires a non-root process")
	}
	s, req := permissionRestoreFixture(t)
	if _, err := s.restorePreview(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	privateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(privateDir, "file.txt"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenRoot(req.Target)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	private, err := os.OpenRoot(privateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	if err := os.Chmod(req.Target, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(req.Target, 0700)
	e := Entry{SourceID: "s", Path: "nested/file.txt", Kind: "file", Size: 4, SnapshotPath: "/file.txt"}
	requireRestorePermissionError(t, s.copyRestored(out, private, e, false, restoreLayoutDirect))
	if _, err := os.Lstat(filepath.Join(req.Target, "nested")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected partial output: %v", err)
	}
}

func TestRestoreReadOnlyFilesystemError(t *testing.T) {
	requireRestorePermissionError(t, restoreTargetError(&os.PathError{Op: "mkdirat", Path: "s", Err: syscall.EROFS}, "/restore/s"))
}
