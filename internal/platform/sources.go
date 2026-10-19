package platform

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// MountIdentity binds a source to its directory inode and filesystem, detecting
// an unmounted volume that exposes the otherwise valid underlying directory.
func MountIdentity(path string) (string, error) {
	actual, e := canonicalDir(path)
	if e != nil {
		return "", e
	}
	info, e := os.Stat(actual)
	if e != nil {
		return "", e
	}
	return directoryIdentity(actual, info)
}
func directoryIdentity(actual string, info os.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("无法获取目录设备身份")
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d", actual, stat.Dev, stat.Ino)))), nil
}

// SourceAccessHint explains a permission error on a backup source. Sources
// belong to other applications such as panels and databases; changing their
// owner or mode for this service can break them, so ask for read access only.
func SourceAccessHint(e error) string {
	if !errors.Is(e, os.ErrPermission) {
		return ""
	}
	return fmt.Sprintf("。当前进程 UID=%d、GID=%d 没有读取权限；请在宿主机只为该用户或组增加只读权限（如只读 ACL），不要用 chown/chmod 修改源目录原有属主或权限，以免影响 1Panel、数据库等原应用", os.Geteuid(), os.Getegid())
}
func (r *Runtime) ValidateSourcePath(path string) (string, error) {
	actual, e := canonicalDir(path)
	if e != nil {
		return "", e
	}
	allowed := false
	for _, root := range r.Config.SourceRoots {
		canonical, e := canonicalDir(root)
		if e != nil || !within(actual, canonical) {
			continue
		}
		// A deployment root may itself have a platform alias (e.g. macOS
		// /tmp). Descendants must resolve directly, without symlink hops.
		absRoot, _ := filepath.Abs(root)
		absPath, _ := filepath.Abs(path)
		base := absRoot
		if !within(absPath, base) {
			base = canonical
		}
		if !within(absPath, base) {
			continue
		}
		rel, e := filepath.Rel(base, absPath)
		if e != nil {
			continue
		}
		if filepath.Join(canonical, rel) != actual {
			continue
		}
		opened, e := os.OpenRoot(canonical)
		if e != nil {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		current := ""
		clean := true
		for _, part := range parts {
			if part == "." || part == "" {
				continue
			}
			current = filepath.Join(current, part)
			info, e := opened.Lstat(current)
			if e != nil || info.Mode()&os.ModeSymlink != 0 {
				clean = false
				break
			}
		}
		opened.Close()
		if clean {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("目录不在部署允许的备份源挂载范围内")
	}
	for _, blocked := range append([]string{r.Config.StateDir, r.Config.CacheDir}, r.Config.RestoreRoots...) {
		canonical, e := canonicalDir(blocked)
		if e == nil && overlap(actual, canonical) {
			return "", fmt.Errorf("备份源不能包含状态、缓存或恢复输出目录")
		}
	}
	f, e := os.Open(actual)
	if e != nil {
		return "", errors.New("目录无法读取" + SourceAccessHint(e))
	}
	defer f.Close()
	if _, e = f.Readdirnames(1); e != nil && e != io.EOF {
		return "", errors.New("目录无法读取" + SourceAccessHint(e))
	}
	return actual, nil
}
func (r *Runtime) saveSource(w http.ResponseWriter, req *http.Request) {
	var s Source
	if !decode(w, req, &s) {
		return
	}
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" || s.MaxFileBytes < 0 {
		fail(w, 400, "invalid_source", "请输入源名称，文件大小限制不能为负数")
		return
	}
	actual, e := r.ValidateSourcePath(s.Path)
	if e != nil {
		fail(w, 400, "source_path_invalid", e.Error())
		return
	}
	identity, e := MountIdentity(actual)
	if e != nil {
		fail(w, 400, "mount_identity_unavailable", "无法读取目录挂载身份")
		return
	}
	s.Path = actual
	s.MountIdentity = identity
	if s.Excludes == nil {
		s.Excludes = []string{}
	}
	id := req.PathValue("id")
	expected := 0
	status := 201
	if id == "" {
		id = newID()
	} else {
		expected = s.Version
		status = 200
		if expected <= 0 {
			fail(w, 400, "version_required", "修改必须提供当前版本")
			return
		}
	}
	s.ID = id
	v, e := r.Store.Save(req.Context(), "sources", id, expected, s)
	s.Version = v
	saved(w, status, e, s)
	if e == nil {
		_ = r.Store.Audit(req.Context(), "source.save", id)
	}
}

type browseEntry struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	MountIdentity string `json:"mount_identity"`
}

func (r *Runtime) browse(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Query().Get("path")
	if path == "" {
		items := []browseEntry{}
		for _, root := range r.Config.SourceRoots {
			actual, e := canonicalDir(root)
			if e != nil {
				continue
			}
			id, _ := MountIdentity(actual)
			items = append(items, browseEntry{filepath.Base(actual), actual, id})
		}
		writeJSON(w, 200, map[string]any{"items": items, "total": len(items), "path": "", "roots": r.Config.SourceRoots})
		return
	}
	actual, e := r.ValidateSourcePath(path)
	if e != nil {
		fail(w, 400, "source_path_invalid", e.Error())
		return
	}
	f, e := os.Open(actual)
	if e != nil {
		fail(w, 400, "source_unreadable", "目录无法读取")
		return
	}
	defer f.Close()
	limit, offset := pagination(req)
	items := []browseEntry{}
	total := 0
	for {
		entries, e := f.ReadDir(256)
		for _, entry := range entries {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			full := filepath.Join(actual, entry.Name())
			if _, e := r.ValidateSourcePath(full); e != nil {
				continue
			}
			if total >= offset && len(items) < limit {
				id, e := MountIdentity(full)
				if e != nil {
					continue
				}
				items = append(items, browseEntry{entry.Name(), full, id})
			}
			total++
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			fail(w, 400, "source_unreadable", "读取目录失败")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "path": actual, "roots": r.Config.SourceRoots})
}
