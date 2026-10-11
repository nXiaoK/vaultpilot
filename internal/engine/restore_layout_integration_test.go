package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRealResticSingleSourceRestoreLayouts(t *testing.T) {
	s, plan, pool, _ := realFixture(t)
	ctx := context.Background()
	pool.CatalogReplicas = 1
	if _, err := s.rt.Store.Save(ctx, "storage_pools", pool.ID, -1, pool); err != nil {
		t.Fatal(err)
	}
	var source Source
	if err := s.rt.Store.Get(ctx, "sources", plan.SourceIDs[0], &source); err != nil {
		t.Fatal(err)
	}
	contents := map[string]string{
		"cn_windows_server_2012_r2.iso": "first image contents",
		"zh-cn_windows_10_ltsc.iso":     "second image contents",
		"nested/file.txt":               "nested contents",
	}
	if err := os.Remove(filepath.Join(source.Path, "file.txt")); err != nil {
		t.Fatal(err)
	}
	for path, value := range contents {
		full := filepath.Join(source.Path, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(value), 0640); err != nil {
			t.Fatal(err)
		}
	}
	dirTime := time.Unix(1_700_000_000, 0)
	nested := filepath.Join(source.Path, "nested")
	if err := os.Chmod(nested, 0710); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(nested, dirTime, dirTime); err != nil {
		t.Fatal(err)
	}
	backup := Task{ID: uid(), Kind: "backup", PlanID: plan.ID, StartedAt: now()}
	if err := s.backup(ctx, &backup); err != nil {
		t.Fatal(err)
	}
	pointID := backup.Result.(map[string]any)["point_id"].(string)
	if err := os.MkdirAll(s.rt.Config.RestoreRoots[0], 0700); err != nil {
		t.Fatal(err)
	}
	restoreRoot, err := filepath.EvalSymlinks(s.rt.Config.RestoreRoots[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, layout, resolved string
		subtree                bool
	}{
		{"default", "", restoreLayoutDirect, false},
		{"direct", restoreLayoutDirect, restoreLayoutDirect, false},
		{"by-source", restoreLayoutBySource, restoreLayoutBySource, false},
		{"subtree", restoreLayoutAuto, restoreLayoutDirect, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := filepath.Join(restoreRoot, tc.name)
			if err := os.Mkdir(target, 0750); err != nil {
				t.Fatal(err)
			}
			req := RestoreRequest{PointID: pointID, Target: target, Layout: tc.layout}
			if tc.subtree {
				req.Paths = []PathSelection{{SourceID: source.ID, Path: "nested"}}
			}
			preview, err := s.restorePreview(ctx, req)
			if err != nil || preview.Blocked || preview.Layout != tc.resolved {
				t.Fatalf("preflight: %+v %v", preview, err)
			}
			output := target
			if tc.resolved == restoreLayoutBySource {
				output = filepath.Join(target, source.ID)
			}
			if len(preview.OutputRoots) != 1 || preview.OutputRoots[0].Path != output {
				t.Fatalf("output mapping: %+v", preview.OutputRoots)
			}
			task, err := s.enqueue("restore", "", "", req)
			if err != nil {
				t.Fatal(err)
			}
			s.execute(task.ID)
			var final Task
			if err := s.rt.Store.Get(ctx, "backup_runs", task.ID, &final); err != nil || final.Status != "completed" {
				t.Fatalf("restore task: %+v %v", final, err)
			}
			result := final.Result.(map[string]any)
			if result["layout"] != tc.resolved {
				t.Fatalf("incorrect report layout: %+v", result)
			}
			for path, want := range contents {
				got, err := os.ReadFile(filepath.Join(output, path))
				if tc.subtree && path != "nested/file.txt" {
					if !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("wrote unselected file %s: %v", path, err)
					}
					continue
				}
				if err != nil || string(got) != want {
					t.Fatalf("restored %s: %q %v", path, got, err)
				}
			}
			if tc.resolved == restoreLayoutDirect {
				if _, err := os.Lstat(filepath.Join(target, source.ID)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unnecessary source ID directory: %v", err)
				}
				info, err := os.Stat(target)
				if err != nil || info.Mode().Perm() != 0750 {
					t.Fatalf("changed target root permissions: %v %v", info, err)
				}
			}
			info, err := os.Stat(filepath.Join(output, "nested"))
			if err != nil || info.Mode().Perm() != 0710 || !info.ModTime().Equal(dirTime) {
				t.Fatalf("nested metadata: %v %v", info, err)
			}
		})
	}
}

func TestPartialRestoreDoesNotChangeUnavailableDirectoryMetadata(t *testing.T) {
	s, req := permissionRestoreFixture(t)
	ctx := context.Background()
	// No account is available, so even existing directories must be left alone.
	if _, err := s.rt.Store.DB.Exec("DELETE FROM accounts"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.rt.Store.DB.Exec("INSERT INTO file_entries(run_id,source_id,path,kind,mode,mtime_ns,repo_id,snapshot_path) VALUES(?,?,?,?,?,?,?,?)", "run", "s", "nested", "dir", 0500, int64(1), "repo", "/nested"); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(req.Target, "nested")
	if err := os.Mkdir(nested, 0750); err != nil {
		t.Fatal(err)
	}
	originalTime := time.Unix(1_600_000_000, 0)
	if err := os.Chtimes(nested, originalTime, originalTime); err != nil {
		t.Fatal(err)
	}
	req.AllowPartial = true
	body, _ := json.Marshal(req)
	task := Task{ID: uid(), Request: body}
	err := s.restore(ctx, &task)
	var f *failure
	if !errors.As(err, &f) || f.Code != "RESTORE_PARTIAL" {
		t.Fatalf("partial restore: %v", err)
	}
	info, err := os.Stat(nested)
	if err != nil || info.Mode().Perm() != 0750 || !info.ModTime().Equal(originalTime) {
		t.Fatalf("unavailable directory metadata modified: %v %v", info, err)
	}
}
