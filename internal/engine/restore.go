package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type PathSelection struct {
	SourceID string `json:"source_id"`
	Path     string `json:"path"`
}
type RestoreRequest struct {
	PointID      string          `json:"point_id"`
	SourceIDs    []string        `json:"source_ids"`
	Paths        []PathSelection `json:"paths"`
	Target       string          `json:"target"`
	Overwrite    bool            `json:"overwrite"`
	AllowPartial bool            `json:"allow_partial"`
}

func (r *RestoreRequest) UnmarshalJSON(b []byte) error {
	type request RestoreRequest
	var raw struct {
		PointID      string          `json:"point_id"`
		SourceIDs    []string        `json:"source_ids"`
		Paths        json.RawMessage `json:"paths"`
		Target       string          `json:"target"`
		Overwrite    bool            `json:"overwrite"`
		AllowPartial bool            `json:"allow_partial"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*r = RestoreRequest{PointID: raw.PointID, SourceIDs: raw.SourceIDs, Target: raw.Target, Overwrite: raw.Overwrite, AllowPartial: raw.AllowPartial}
	if len(raw.Paths) == 0 || string(raw.Paths) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw.Paths, &r.Paths); err == nil {
		return nil
	}
	r.Paths = nil
	var paths []string
	if err := json.Unmarshal(raw.Paths, &paths); err != nil {
		return err
	}
	for _, p := range paths {
		parts := strings.SplitN(p, "/", 2)
		selection := PathSelection{SourceID: parts[0]}
		if len(parts) > 1 {
			selection.Path = parts[1]
		}
		r.Paths = append(r.Paths, selection)
	}
	return nil
}

type RestorePreview struct {
	FileCount        int64           `json:"file_count"`
	LogicalBytes     int64           `json:"logical_bytes"`
	ScratchBytes     int64           `json:"scratch_bytes"`
	RequiredBytes    int64           `json:"required_bytes"`
	Conflicts        []PathSelection `json:"conflicts"`
	ConflictCount    int64           `json:"conflict_count"`
	MissingAccounts  []string        `json:"missing_accounts"`
	MissingFiles     []PathSelection `json:"missing_files"`
	MissingFileCount int64           `json:"missing_file_count"`
	Target           string          `json:"target"`
	AllowPartial     bool            `json:"allow_partial"`
	Blocked          bool            `json:"blocked"`
	Code             string          `json:"code,omitempty"`
}

func safeID(id string) bool {
	return id != "" && !strings.ContainsAny(id, "/\\\x00") && id != "." && id != ".."
}
func safeLogical(path string) bool {
	if strings.ContainsRune(path, 0) || strings.HasPrefix(path, "/") {
		return false
	}
	if path == "" {
		return true
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
func selectionWhere(req RestoreRequest, run string) (string, []any, error) {
	where := "run_id=?"
	args := []any{run}
	if len(req.SourceIDs) > 0 {
		parts := []string{}
		for _, id := range req.SourceIDs {
			if !safeID(id) {
				return "", nil, fail("INVALID_SELECTION", "源标识无效")
			}
			parts = append(parts, "?")
			args = append(args, id)
		}
		where += " AND source_id IN (" + strings.Join(parts, ",") + ")"
	}
	if len(req.Paths) > 0 {
		parts := []string{}
		for _, p := range req.Paths {
			if !safeID(p.SourceID) || !safeLogical(p.Path) {
				return "", nil, fail("INVALID_SELECTION", "恢复路径无效")
			}
			if p.Path == "" {
				parts = append(parts, "source_id=?")
				args = append(args, p.SourceID)
			} else {
				parts = append(parts, "(source_id=? AND (path=? OR substr(path,1,?)=?))")
				args = append(args, p.SourceID, p.Path, utf8.RuneCountInString(p.Path)+1, p.Path+"/")
			}
		}
		where += " AND (" + strings.Join(parts, " OR ") + ")"
	}
	return where, args, nil
}
func (s *Service) allowedTarget(target string, create bool) (*os.Root, string, error) {
	if !filepath.IsAbs(target) {
		return nil, "", fail("RESTORE_BOUNDARY", "恢复目标必须是绝对路径")
	}
	target = filepath.Clean(target)
	for _, allowed := range s.rt.Config.RestoreRoots {
		base, err := filepath.EvalSymlinks(allowed)
		if err != nil {
			continue
		}
		if !within(base, target) {
			continue
		}
		root, err := os.OpenRoot(base)
		if err != nil {
			return nil, "", restoreTargetError(err, base)
		}
		rel, err := filepath.Rel(base, target)
		if err != nil {
			root.Close()
			return nil, "", err
		}
		if rel == "." {
			return root, target, nil
		}
		if create {
			if err = root.MkdirAll(rel, 0700); err != nil {
				root.Close()
				return nil, "", restoreTargetError(err, target)
			}
		}
		child, err := root.OpenRoot(rel)
		if err == nil {
			root.Close()
			return child, target, nil
		}
		if !create && errors.Is(err, os.ErrNotExist) {
			return root, target, nil
		}
		root.Close()
		if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) {
			return nil, "", restoreTargetError(err, target)
		}
		return nil, "", fail("RESTORE_BOUNDARY", "恢复目标含越界链接或不可写路径")
	}
	return nil, "", fail("RESTORE_BOUNDARY", "目标不在部署允许的恢复目录内")
}
func (s *Service) restoreContext(ctx context.Context, req RestoreRequest) (Point, string, []any, map[string]Repository, map[string]Account, error) {
	var p Point
	if err := s.rt.Store.Get(ctx, "recovery_points", req.PointID, &p); err != nil {
		return p, "", nil, nil, nil, err
	}
	if p.Status != "completed" || p.DeletedAt != "" {
		return p, "", nil, nil, nil, fail("POINT_UNAVAILABLE", "恢复点未完整提交或已删除")
	}
	where, args, err := selectionWhere(req, p.RunID)
	if err != nil {
		return p, "", nil, nil, nil, err
	}
	repos := map[string]Repository{}
	accounts := map[string]Account{}
	raws, err := s.rt.Store.All(ctx, "repositories")
	if err != nil {
		return p, "", nil, nil, nil, err
	}
	for _, raw := range raws {
		var repo Repository
		_ = json.Unmarshal(raw, &repo)
		repos[repo.ID] = repo
	}
	raws, err = s.rt.Store.All(ctx, "accounts")
	if err != nil {
		return p, "", nil, nil, nil, err
	}
	for _, raw := range raws {
		var a Account
		_ = json.Unmarshal(raw, &a)
		if a.Verified && a.Status != "error" {
			if a.Backend == "local" {
				if _, err = os.Stat(a.LocalPath); err != nil {
					continue
				}
			}
			accounts[a.ID] = a
		}
	}
	return p, where, args, repos, accounts, nil
}
func (s *Service) restorePreview(ctx context.Context, req RestoreRequest) (RestorePreview, error) {
	preview := RestorePreview{Conflicts: []PathSelection{}, MissingAccounts: []string{}, MissingFiles: []PathSelection{}, Target: req.Target, AllowPartial: req.AllowPartial}
	root, target, err := s.allowedTarget(req.Target, false)
	if err != nil {
		return preview, err
	}
	defer root.Close()
	preview.Target = target
	targetRel, err := filepath.Rel(root.Name(), target)
	if err != nil {
		return preview, err
	}
	checkedDirectories := map[string]bool{}
	if err = checkRestoreDirectoryWritable(root, targetRel, checkedDirectories); err != nil {
		return preview, err
	}
	point, where, args, repos, accounts, err := s.restoreContext(ctx, req)
	if err != nil {
		return preview, err
	}
	var stat syscall.Statfs_t
	if err = syscall.Statfs(root.Name(), &stat); err != nil {
		return preview, err
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	missing := map[string]bool{}
	maxFile := int64(0)
	offset := 0
	for {
		entries, err := s.entries(ctx, where, args, 512, offset)
		if err != nil {
			return preview, err
		}
		if len(entries) == 0 {
			break
		}
		offset += len(entries)
		for _, e := range entries {
			preview.FileCount++
			preview.LogicalBytes += e.Size
			if e.Size > maxFile {
				maxFile = e.Size
			}
			repo, ok := repos[e.RepositoryID]
			_, ready := accounts[repo.AccountID]
			if !ok || !ready {
				preview.MissingFileCount++
				if len(preview.MissingFiles) < 200 {
					preview.MissingFiles = append(preview.MissingFiles, PathSelection{e.SourceID, e.Path})
				}
				missing[repo.AccountID] = true
				continue
			}
			rel := filepath.Join(e.SourceID, filepath.FromSlash(e.Path)) // Query only; no target writes during preview.
			if err = checkRestoreDirectoryWritable(root, filepath.Dir(filepath.Join(targetRel, rel)), checkedDirectories); err != nil {
				return preview, err
			}
			path := filepath.Join(target, rel)
			_, err := os.Lstat(path)
			if err == nil {
				if e.Kind == "dir" {
					info, err := os.Lstat(path)
					if err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
						continue
					}
				}
				preview.ConflictCount++
				if len(preview.Conflicts) < 200 {
					preview.Conflicts = append(preview.Conflicts, PathSelection{e.SourceID, e.Path})
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return preview, restoreTargetError(err, path)
			}
		}
	}
	for id := range missing {
		preview.MissingAccounts = append(preview.MissingAccounts, id)
	}
	scratch := int64(64 << 20)
	if preview.LogicalBytes < scratch {
		scratch = preview.LogicalBytes
	}
	if maxFile > scratch {
		scratch = maxFile
	}
	cacheUsage := int64(0)
	if err = filepath.WalkDir(s.rt.Config.CacheDir, func(path string, d fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if d.Type().IsRegular() {
			info, e := d.Info()
			if errors.Is(e, os.ErrNotExist) {
				return nil
			}
			if e != nil {
				return e
			}
			cacheUsage += info.Size()
		}
		return nil
	}); err != nil {
		return preview, err
	}
	if limit := s.rt.Config.CacheMaxBytes; limit > 0 && (maxFile > limit || cacheUsage+scratch > limit) {
		preview.Blocked = true
		preview.Code = "CACHE_LIMIT_EXCEEDED"
	}
	preview.ScratchBytes = scratch
	preview.RequiredBytes = preview.LogicalBytes
	var cacheStat syscall.Statfs_t
	if err = syscall.Statfs(s.rt.Config.CacheDir, &cacheStat); err != nil {
		return preview, err
	}
	cacheAvailable := int64(cacheStat.Bavail) * int64(cacheStat.Bsize)
	var rootInfo, cacheInfo syscall.Stat_t
	if syscall.Stat(root.Name(), &rootInfo) == nil && syscall.Stat(s.rt.Config.CacheDir, &cacheInfo) == nil && rootInfo.Dev == cacheInfo.Dev {
		preview.RequiredBytes += scratch
	} else if scratch > cacheAvailable {
		preview.Blocked = true
		preview.Code = "RESTORE_CACHE_SPACE_INSUFFICIENT"
	}
	if point.ProtectionStatus == "degraded" && !req.AllowPartial {
		preview.Blocked = true
		preview.Code = "PROTECTION_DEGRADED"
	}
	if preview.FileCount == 0 {
		preview.Blocked = true
		preview.Code = "EMPTY_SELECTION"
	}
	if preview.RequiredBytes > available {
		preview.Blocked = true
		preview.Code = "RESTORE_SPACE_INSUFFICIENT"
	}
	if preview.ConflictCount > 0 && !req.Overwrite {
		preview.Blocked = true
		preview.Code = "RESTORE_CONFLICT"
	}
	if preview.MissingFileCount > 0 && !req.AllowPartial {
		preview.Blocked = true
		preview.Code = "RESTORE_ACCOUNT_MISSING"
	}
	return preview, nil
}
func (s *Service) restorePreviewHTTP(w http.ResponseWriter, r *http.Request) {
	unlockIndex := s.lock("recovery-index")
	defer unlockIndex()
	var req RestoreRequest
	if err := decode(r, &req); err != nil {
		problem(w, err)
		return
	}
	preview, err := s.restorePreview(r.Context(), req)
	if err != nil {
		problem(w, err)
		return
	}
	write(w, 200, preview)
}
func globLiteral(path string) string {
	r := strings.NewReplacer("\\", "\\\\", "*", "\\*", "?", "\\?", "[", "\\[")
	return r.Replace(path)
}
func assertParents(root *os.Root, path string) error {
	parts := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
	current := ""
	for _, part := range parts {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err = root.Mkdir(current, 0700); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fail("RESTORE_UNSAFE_PARENT", "恢复路径的父目录不是普通目录")
		}
	}
	return nil
}
func sourceSnapshotRelative(path string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		return "", fail("SNAPSHOT_PATH_INVALID", "快照路径必须是绝对路径")
	}
	rel := strings.TrimPrefix(path, "/")
	if !safeLogical(rel) {
		return "", fail("SNAPSHOT_PATH_INVALID", "快照路径无效")
	}
	return filepath.FromSlash(rel), nil
}
func safeLink(e Entry) bool {
	if filepath.IsAbs(e.LinkTarget) {
		return false
	}
	resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(e.Path), e.LinkTarget)))
	return resolved != ".." && !strings.HasPrefix(resolved, "../")
}
func setFileTimes(file *os.File, ns int64) error {
	tv := unix.NsecToTimeval(ns)
	return unix.Futimes(int(file.Fd()), []unix.Timeval{tv, tv})
}
func (s *Service) copyRestored(out, private *os.Root, e Entry, overwrite bool) error {
	if !safeID(e.SourceID) || !safeLogical(e.Path) {
		return fail("RESTORE_PATH_INVALID", "恢复清单路径无效")
	}
	target := filepath.Join(e.SourceID, filepath.FromSlash(e.Path))
	outputPath := filepath.Join(out.Name(), target)
	if err := assertParents(out, target); err != nil {
		return restoreTargetError(err, outputPath)
	}
	snapshotPath, err := sourceSnapshotRelative(e.SnapshotPath)
	if err != nil {
		return err
	}
	info, err := private.Lstat(snapshotPath)
	if err != nil {
		return err
	}
	existing, statErr := out.Lstat(target)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return restoreTargetError(statErr, outputPath)
	}
	if e.Kind == "dir" {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fail("RESTORE_TYPE_MISMATCH", "恢复目录类型不符")
		}
		if exists {
			if existing.IsDir() && existing.Mode()&os.ModeSymlink == 0 {
				return nil
			}
			if !overwrite {
				return fail("RESTORE_CONFLICT", "恢复目标已存在")
			}
			return fail("RESTORE_CONFLICT", "拒绝将现有非目录目标替换为目录")
		}
		return restoreTargetError(out.Mkdir(target, 0700), outputPath)
	}
	if exists && !overwrite {
		return fail("RESTORE_CONFLICT", "恢复目标已存在")
	}
	if exists && existing.IsDir() {
		return fail("RESTORE_CONFLICT", "拒绝覆盖目标目录")
	}
	if e.Kind == "symlink" {
		if info.Mode()&os.ModeSymlink == 0 {
			return fail("RESTORE_TYPE_MISMATCH", "恢复链接类型不符")
		}
		link, err := private.Readlink(snapshotPath)
		if err != nil {
			return err
		}
		if link != e.LinkTarget {
			return fail("RESTORE_LINK_MISMATCH", "恢复链接内容与清单不符")
		}
		if !safeLink(e) {
			return fail("RESTORE_UNSAFE_LINK", "拒绝恢复指向源边界外的符号链接："+e.Path)
		}
		if !overwrite {
			return restoreTargetError(out.Symlink(link, target), outputPath)
		}
		temp := filepath.Join(filepath.Dir(target), ".vaultpilot-"+uid())
		if err = out.Symlink(link, temp); err != nil {
			return restoreTargetError(err, outputPath)
		}
		defer out.Remove(temp)
		return restoreTargetError(out.Rename(temp, target), outputPath)
	}
	if e.Kind != "file" || !info.Mode().IsRegular() {
		return fail("RESTORE_TYPE_MISMATCH", "恢复文件类型不符")
	}
	in, err := private.Open(snapshotPath)
	if err != nil {
		return err
	}
	defer in.Close()
	temp := filepath.Join(filepath.Dir(target), ".vaultpilot-"+uid())
	file, err := out.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return restoreTargetError(err, outputPath)
	}
	defer out.Remove(temp)
	n, err := io.Copy(file, in)
	if err == nil && n != e.Size {
		err = fail("RESTORE_SIZE_MISMATCH", "恢复文件大小与清单不符")
	}
	if err == nil {
		err = file.Chmod(os.FileMode(e.Mode).Perm())
	}
	if err == nil {
		err = setFileTimes(file, e.MtimeNS)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return restoreTargetError(err, outputPath)
	}
	if closeErr != nil {
		return restoreTargetError(closeErr, outputPath)
	}
	if !overwrite {
		if err = out.Link(temp, target); err != nil {
			if errors.Is(err, os.ErrExist) {
				return fail("RESTORE_CONFLICT", "写入时目标已存在，未覆盖")
			}
			return restoreTargetError(err, outputPath)
		}
		return nil
	}
	return restoreTargetError(out.Rename(temp, target), outputPath)
}
func (s *Service) restore(ctx context.Context, t *Task) (resultErr error) {
	unlockIndex := s.lock("recovery-index")
	defer unlockIndex()
	var req RestoreRequest
	if err := json.Unmarshal(t.Request, &req); err != nil {
		return err
	}
	preview, err := s.restorePreview(ctx, req)
	if err != nil {
		return err
	}
	if preview.Blocked {
		if preview.Code == "CACHE_LIMIT_EXCEEDED" {
			return fail(preview.Code, "当前缓存占用与预计临时恢复片段超过 VP_CACHE_MAX_BYTES，请增大缓存预算或选择更小范围")
		}
		return fail(preview.Code, "恢复预检查未通过，请查看冲突、空间及缺失范围")
	}
	restored, skipped, failed, conflicts := int64(0), preview.MissingFileCount, int64(0), int64(0)
	verificationErrors := int64(0)
	verificationUnknown := false
	failures := []map[string]any{}
	failureCount := int64(0)
	defer func() {
		remaining := preview.FileCount - restored - skipped - failed - conflicts
		if remaining < 0 {
			remaining = 0
		}
		verificationStatus := "passed"
		if resultErr != nil && restored == 0 && failed == 0 {
			verificationStatus = "not_started"
		} else if remaining > 0 && resultErr != nil {
			verificationStatus = "partial"
		}
		var verificationCount any = verificationErrors
		if verificationUnknown {
			verificationStatus = "unknown"
			verificationCount = nil
		} else if verificationErrors > 0 {
			verificationStatus = "failed"
		}
		t.Result = map[string]any{"restored": restored, "skipped": skipped, "conflicts": conflicts, "failed": failed, "unattempted": remaining, "verification_errors": verificationCount, "verification_status": verificationStatus, "failure_entries": failures, "failure_entry_count": failureCount, "failure_entries_truncated": failureCount > int64(len(failures)), "target": req.Target, "point_id": req.PointID, "verification": "Restic --verify + file size + link target"}
		if resultErr != nil && restored > 0 {
			t.partialRestore = true
		}
	}()
	point, where, args, repos, accounts, err := s.restoreContext(ctx, req)
	if err != nil {
		return err
	}
	out, _, err := s.allowedTarget(req.Target, true)
	if err != nil {
		return err
	}
	defer out.Close()
	privateDir, err := os.MkdirTemp(s.rt.Config.CacheDir, "restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(privateDir)
	private, err := os.OpenRoot(privateDir)
	if err != nil {
		return err
	}
	defer private.Close()
	s.stage(t, "restoring")
	for _, snap := range point.Snapshots {
		if snap.Empty {
			continue
		}
		repo, ok := repos[snap.RepositoryID]
		if !ok {
			continue
		}
		account, ready := accounts[repo.AccountID]
		if !ready {
			continue
		}
		unlockAccount := s.lock("account:" + account.ID)
		unlockRepo := s.lock("repo:" + repo.ID)
		offset := 0
		for {
			if e := s.rt.CheckCacheBudget(); e != nil {
				unlockRepo()
				unlockAccount()
				return fail("CACHE_LIMIT_EXCEEDED", e.Error())
			}
			filter := where + " AND repo_id=?"
			filterArgs := append(append([]any{}, args...), repo.ID)
			entries, err := s.entries(ctx, filter, filterArgs, 128, offset)
			if err != nil {
				unlockRepo()
				unlockAccount()
				return err
			}
			if len(entries) == 0 {
				break
			}
			batchBytes := int64(0)
			take := 0
			for _, entry := range entries {
				if take > 0 && batchBytes+entry.Size > 64<<20 {
					break
				}
				batchBytes += entry.Size
				take++
			}
			entries = entries[:take]
			offset += len(entries)
			restoreArgs := []string{"restore", snap.SnapshotID, "--target", privateDir, "--verify"}
			for _, e := range entries {
				restoreArgs = append(restoreArgs, "--include", globLiteral(e.SnapshotPath))
			}
			if _, err = s.restic(ctx, t, repo, account, restoreArgs, func(line string) {
				var msg struct {
					Type string `json:"message_type"`
				}
				if json.Unmarshal([]byte(line), &msg) != nil || msg.Type == "error" {
					s.log(t, line)
				}
			}); err != nil {
				unlockRepo()
				unlockAccount()
				failed += int64(len(entries))
				verificationUnknown = true
				for _, entry := range entries {
					failureCount++
					if len(failures) < 200 {
						failures = append(failures, map[string]any{"source_id": entry.SourceID, "path": entry.Path, "code": "RESTORE_ENGINE_FAILED", "stage": "engine_restore_or_verify"})
					}
				}
				return fail("RESTORE_ENGINE_FAILED", "Restic 恢复或校验失败："+err.Error())
			}
			for _, e := range entries {
				if err = s.copyRestored(out, private, e, req.Overwrite); err != nil {
					var f *failure
					code := "RESTORE_WRITE_FAILED"
					if errors.As(err, &f) {
						code = f.Code
					}
					if code == "RESTORE_CONFLICT" {
						conflicts++
					} else {
						failed++
					}
					if code == "RESTORE_SIZE_MISMATCH" || code == "RESTORE_LINK_MISMATCH" || code == "RESTORE_TYPE_MISMATCH" {
						verificationErrors++
					}
					failureCount++
					if len(failures) < 200 {
						failures = append(failures, map[string]any{"source_id": e.SourceID, "path": e.Path, "code": code, "stage": "output_write"})
					}
					unlockRepo()
					unlockAccount()
					return err
				}
				if e.Kind != "dir" {
					relative, _ := sourceSnapshotRelative(e.SnapshotPath)
					_ = private.Remove(relative)
				}
				restored++
				t.Progress = float64(restored) / float64(preview.FileCount)
				if e.Kind == "file" {
					t.UploadedBytes += e.Size
				}
			}
			s.save(t)
		}
		unlockRepo()
		unlockAccount()
	}
	// Apply directory permissions and timestamps after every descendant is written.
	rows, err := s.rt.Store.DB.QueryContext(ctx, "SELECT source_id,path,mode,mtime_ns FROM file_entries WHERE "+where+" AND kind=\x27dir\x27 ORDER BY length(path) DESC,path DESC", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var source, path string
		var mode uint32
		var ns int64
		if err = rows.Scan(&source, &path, &mode, &ns); err != nil {
			rows.Close()
			return err
		}
		file, e := out.Open(filepath.Join(source, filepath.FromSlash(path)))
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			rows.Close()
			failureCount++
			if len(failures) < 200 {
				failures = append(failures, map[string]any{"source_id": source, "path": path, "code": "RESTORE_METADATA_FAILED", "stage": "directory_metadata"})
			}
			return e
		}
		e = file.Chmod(os.FileMode(mode).Perm())
		if e == nil {
			e = setFileTimes(file, ns)
		}
		file.Close()
		if e != nil {
			rows.Close()
			failureCount++
			if len(failures) < 200 {
				failures = append(failures, map[string]any{"source_id": source, "path": path, "code": "RESTORE_METADATA_FAILED", "stage": "directory_metadata"})
			}
			return e
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	_ = s.rt.Store.Audit(ctx, "restore.complete", point.ID)
	if skipped > 0 {
		return fail("RESTORE_PARTIAL", "部分恢复完成，缺失账号中的文件已跳过")
	}
	return nil
}
