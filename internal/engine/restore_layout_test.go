package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func requireLayoutError(t *testing.T, err error, code string) {
	t.Helper()
	var failureValue *failure
	if !errors.As(err, &failureValue) || failureValue.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

func insertLayoutEntry(t *testing.T, s *Service, e Entry) {
	t.Helper()
	_, err := s.rt.Store.DB.Exec("INSERT INTO file_entries(run_id,source_id,path,kind,size,mode,mtime_ns,repo_id,snapshot_path,link_target) VALUES(?,?,?,?,?,?,?,?,?,?)", "run", e.SourceID, e.Path, e.Kind, e.Size, e.Mode, e.MtimeNS, e.RepositoryID, e.SnapshotPath, e.LinkTarget)
	if err != nil {
		t.Fatal(err)
	}
}

func layoutPrivateRoot(t *testing.T, files map[string]string) *os.Root {
	t.Helper()
	dir := t.TempDir()
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func openLayoutOutput(t *testing.T, target string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func TestRestoreLayoutSingleSourceAutoAndExplicitCompatibility(t *testing.T) {
	for _, requested := range []string{"", restoreLayoutAuto, restoreLayoutDirect, restoreLayoutBySource} {
		name := requested
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			s, req := permissionRestoreFixture(t)
			req.Layout = requested
			preview, err := s.restorePreview(context.Background(), req)
			if err != nil || preview.Blocked {
				t.Fatalf("preview: %+v %v", preview, err)
			}
			layout, output := restoreLayoutDirect, req.Target
			if requested == restoreLayoutBySource {
				layout, output = restoreLayoutBySource, filepath.Join(req.Target, "s")
			}
			if preview.Layout != layout || len(preview.OutputRoots) != 1 || preview.OutputRoots[0].SourceID != "s" || preview.OutputRoots[0].Path != output {
				t.Fatalf("wrong resolved output roots: %+v", preview)
			}
			out := openLayoutOutput(t, req.Target)
			private := layoutPrivateRoot(t, map[string]string{"file.txt": "data"})
			e := Entry{SourceID: "s", Path: "nested/file.txt", Kind: "file", Size: 4, Mode: 0600, SnapshotPath: "/file.txt"}
			if err = s.copyRestored(out, private, e, false, preview.Layout); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(output, "nested", "file.txt"))
			if err != nil || string(content) != "data" {
				t.Fatalf("actual output disagrees with preview: %q %v", content, err)
			}
			if layout == restoreLayoutDirect {
				if _, err = os.Lstat(filepath.Join(req.Target, "s")); !os.IsNotExist(err) {
					t.Fatal("single-source direct output introduced an unnecessary source ID directory")
				}
			}
		})
	}
}

func TestRestoreLayoutMultiSourceAutoKeepsSameNameFilesSeparate(t *testing.T) {
	s, req := permissionRestoreFixture(t)
	insertLayoutEntry(t, s, Entry{SourceID: "second", Path: "nested/file.txt", Kind: "file", Size: 4, RepositoryID: "repo", SnapshotPath: "/second.txt"})
	preview, err := s.restorePreview(context.Background(), req)
	if err != nil || preview.Blocked || preview.Layout != restoreLayoutBySource || len(preview.OutputRoots) != 2 {
		t.Fatalf("multi-source layout: %+v %v", preview, err)
	}
	out := openLayoutOutput(t, req.Target)
	private := layoutPrivateRoot(t, map[string]string{"first.txt": "one!", "second.txt": "two!"})
	for _, e := range []Entry{
		{SourceID: "s", Path: "nested/file.txt", Kind: "file", Size: 4, Mode: 0600, SnapshotPath: "/first.txt"},
		{SourceID: "second", Path: "nested/file.txt", Kind: "file", Size: 4, Mode: 0600, SnapshotPath: "/second.txt"},
	} {
		if err = s.copyRestored(out, private, e, false, preview.Layout); err != nil {
			t.Fatal(err)
		}
	}
	for source, expected := range map[string]string{"s": "one!", "second": "two!"} {
		content, err := os.ReadFile(filepath.Join(req.Target, source, "nested", "file.txt"))
		if err != nil || string(content) != expected {
			t.Fatalf("cross-source output collision: source=%s content=%q error=%v", source, content, err)
		}
	}
	if _, err = os.Stat(filepath.Join(req.Target, "nested")); !os.IsNotExist(err) {
		t.Fatal("multi-source output was flattened into a shared directory")
	}
	req.Layout = restoreLayoutDirect
	_, err = s.restorePreview(context.Background(), req)
	requireLayoutError(t, err, "RESTORE_LAYOUT_REQUIRES_SINGLE_SOURCE")
}

func TestRestoreLayoutDirectRejectsAllSelectedSourcesBeforeOutputCreation(t *testing.T) {
	for _, kind := range []string{"file", "dir"} {
		t.Run(kind, func(t *testing.T) {
			s, req := permissionRestoreFixture(t)
			repo := Repository{ID: "missing-repo", AccountID: "missing-account"}
			if _, err := s.rt.Store.Save(context.Background(), "repositories", repo.ID, 0, repo); err != nil {
				t.Fatal(err)
			}
			path, size := "nested/file.txt", int64(4)
			if kind == "dir" {
				path, size = "", 0
			}
			insertLayoutEntry(t, s, Entry{SourceID: "unavailable-source", Path: path, Kind: kind, Size: size, RepositoryID: repo.ID, SnapshotPath: "/missing"})
			req.AllowPartial = true
			req.Target = filepath.Join(req.Target, "new-target")
			preview, err := s.restorePreview(context.Background(), req)
			if err != nil || preview.Blocked || preview.Layout != restoreLayoutBySource || len(preview.OutputRoots) != 2 || preview.MissingFileCount != 1 {
				t.Fatalf("missing source changed auto layout: %+v %v", preview, err)
			}
			req.Layout = restoreLayoutDirect
			_, err = s.restorePreview(context.Background(), req)
			requireLayoutError(t, err, "RESTORE_LAYOUT_REQUIRES_SINGLE_SOURCE")
			body, _ := json.Marshal(req)
			task := Task{ID: "rejected-layout", Request: body}
			requireLayoutError(t, s.restore(context.Background(), &task), "RESTORE_LAYOUT_REQUIRES_SINGLE_SOURCE")
			if _, err = os.Lstat(req.Target); !os.IsNotExist(err) {
				t.Fatal("rejected multi-source direct request created the target")
			}
		})
	}
}

func TestRestoreLayoutUsesSelectionIntersectionAndPreservesSubtreePath(t *testing.T) {
	s, req := permissionRestoreFixture(t)
	insertLayoutEntry(t, s, Entry{SourceID: "s", Path: "中文/子目录/a.txt", Kind: "file", Size: 4, RepositoryID: "repo", SnapshotPath: "/unicode.txt"})
	insertLayoutEntry(t, s, Entry{SourceID: "second", Path: "中文/子目录/a.txt", Kind: "file", Size: 4, RepositoryID: "repo", SnapshotPath: "/second.txt"})
	req.SourceIDs = []string{"s", "s"}
	req.Paths = []PathSelection{{SourceID: "s", Path: "中文"}, {SourceID: "second", Path: "中文"}}
	preview, err := s.restorePreview(context.Background(), req)
	if err != nil || preview.Blocked || preview.Layout != restoreLayoutDirect || preview.FileCount != 1 || len(preview.OutputRoots) != 1 || preview.OutputRoots[0].SourceID != "s" {
		t.Fatalf("layout used entire point or selection union: %+v %v", preview, err)
	}
	out := openLayoutOutput(t, req.Target)
	private := layoutPrivateRoot(t, map[string]string{"unicode.txt": "data"})
	e := Entry{SourceID: "s", Path: "中文/子目录/a.txt", Kind: "file", Size: 4, Mode: 0600, SnapshotPath: "/unicode.txt"}
	if err = s.copyRestored(out, private, e, false, preview.Layout); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(req.Target, "中文", "子目录", "a.txt")); err != nil || string(content) != "data" {
		t.Fatalf("subtree was flattened: %q %v", content, err)
	}
	if _, err = os.Stat(filepath.Join(req.Target, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("selected subtree lost its original relative parent directories")
	}
}

func TestRestoreLayoutPreviewChecksActualDestinationWithoutWriting(t *testing.T) {
	for _, layout := range []string{restoreLayoutAuto, restoreLayoutBySource} {
		t.Run(layout, func(t *testing.T) {
			s, req := permissionRestoreFixture(t)
			req.Layout = layout
			output := req.Target
			if layout == restoreLayoutBySource {
				output = filepath.Join(output, "s")
			}
			existing := filepath.Join(output, "nested", "file.txt")
			if err := os.MkdirAll(filepath.Dir(existing), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(existing, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			preview, err := s.restorePreview(context.Background(), req)
			if err != nil || !preview.Blocked || preview.Code != "RESTORE_CONFLICT" || preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0].SourceID != "s" || preview.Conflicts[0].Path != "nested/file.txt" {
				t.Fatalf("actual destination conflict missed: %+v %v", preview, err)
			}
			content, err := os.ReadFile(existing)
			if err != nil || string(content) != "original" {
				t.Fatal("preview modified an existing file")
			}
			req.Target = filepath.Join(req.Target, "not-created")
			preview, err = s.restorePreview(context.Background(), req)
			if err != nil || preview.Blocked {
				t.Fatalf("missing target preview: %+v %v", preview, err)
			}
			if _, err = os.Stat(req.Target); !os.IsNotExist(err) {
				t.Fatal("layout preview created the target directory")
			}
		})
	}
}

func TestRestoreLayoutDirectRetainsFailureAndBoundarySafety(t *testing.T) {
	t.Run("failed size verification preserves existing target", func(t *testing.T) {
		outDir := t.TempDir()
		original := filepath.Join(outDir, "file.txt")
		if err := os.WriteFile(original, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		out := openLayoutOutput(t, outDir)
		private := layoutPrivateRoot(t, map[string]string{"file.txt": "wrong-size"})
		e := Entry{SourceID: "s", Path: "file.txt", Kind: "file", Size: 1, Mode: 0600, SnapshotPath: "/file.txt"}
		s := &Service{}
		requireLayoutError(t, s.copyRestored(out, private, e, true, restoreLayoutDirect), "RESTORE_SIZE_MISMATCH")
		content, err := os.ReadFile(original)
		if err != nil || string(content) != "original" {
			t.Fatal("failed direct restore replaced the original file")
		}
		e.Size = int64(len("wrong-size"))
		requireLayoutError(t, s.copyRestored(out, private, e, false, restoreLayoutDirect), "RESTORE_CONFLICT")
		content, err = os.ReadFile(original)
		if err != nil || string(content) != "original" {
			t.Fatal("non-overwrite direct restore replaced the original file")
		}
	})
	t.Run("symlink parent cannot escape", func(t *testing.T) {
		outDir, outside := t.TempDir(), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(outDir, "nested")); err != nil {
			t.Fatal(err)
		}
		out := openLayoutOutput(t, outDir)
		private := layoutPrivateRoot(t, map[string]string{"file.txt": "data"})
		e := Entry{SourceID: "s", Path: "nested/file.txt", Kind: "file", Size: 4, Mode: 0600, SnapshotPath: "/file.txt"}
		requireLayoutError(t, (&Service{}).copyRestored(out, private, e, true, restoreLayoutDirect), "RESTORE_UNSAFE_PARENT")
		if _, err := os.Stat(filepath.Join(outside, "file.txt")); !os.IsNotExist(err) {
			t.Fatal("direct restore wrote through an escaping parent link")
		}
	})
	t.Run("unsafe restored link cannot escape", func(t *testing.T) {
		outDir := t.TempDir()
		out := openLayoutOutput(t, outDir)
		private := layoutPrivateRoot(t, nil)
		if err := private.Symlink("../../outside", "link"); err != nil {
			t.Fatal(err)
		}
		e := Entry{SourceID: "s", Path: "nested/link", Kind: "symlink", LinkTarget: "../../outside", SnapshotPath: "/link"}
		requireLayoutError(t, (&Service{}).copyRestored(out, private, e, false, restoreLayoutDirect), "RESTORE_UNSAFE_LINK")
		if _, err := os.Lstat(filepath.Join(outDir, "nested", "link")); !os.IsNotExist(err) {
			t.Fatal("unsafe link was published in direct output")
		}
	})
	t.Run("relative link inside source remains valid", func(t *testing.T) {
		outDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(outDir, "file.txt"), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
		out := openLayoutOutput(t, outDir)
		private := layoutPrivateRoot(t, nil)
		if err := private.Symlink("../file.txt", "link"); err != nil {
			t.Fatal(err)
		}
		e := Entry{SourceID: "s", Path: "nested/link", Kind: "symlink", LinkTarget: "../file.txt", SnapshotPath: "/link"}
		if err := (&Service{}).copyRestored(out, private, e, false, restoreLayoutDirect); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(outDir, "nested", "link"))
		if err != nil || string(content) != "data" {
			t.Fatalf("safe source-relative link changed under direct output: %q %v", content, err)
		}
	})
}

func TestRestoreLayoutJSONAndInvalidInput(t *testing.T) {
	var req RestoreRequest
	if err := json.Unmarshal([]byte(`{"point_id":"p","layout":"by_source","paths":["s/中文/a.txt"]}`), &req); err != nil || req.Layout != restoreLayoutBySource || len(req.Paths) != 1 || req.Paths[0].Path != "中文/a.txt" {
		t.Fatalf("legacy path JSON lost explicit layout: %+v %v", req, err)
	}
	s, request := permissionRestoreFixture(t)
	request.Layout = "unexpected"
	_, err := s.restorePreview(context.Background(), request)
	requireLayoutError(t, err, "INVALID_RESTORE_LAYOUT")
	for _, e := range []Entry{
		{SourceID: "../outside", Path: "file", Kind: "file"},
		{SourceID: "s", Path: "../outside", Kind: "file"},
		{SourceID: "s", Path: "/absolute", Kind: "file"},
		{SourceID: "s", Path: "", Kind: "file"},
	} {
		_, err = restoreOutputPath(e, restoreLayoutDirect)
		requireLayoutError(t, err, "RESTORE_PATH_INVALID")
	}
}
