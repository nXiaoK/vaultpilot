package platform

import (
	"container/heap"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

type sourceEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Kind       string    `json:"kind"`
	Size       *int64    `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	Mode       string    `json:"mode"`
	CanOpen    bool      `json:"can_open"`
	Reason     string    `json:"reason,omitempty"`
	LinkTarget string    `json:"link_target,omitempty"`
}

func entryKey(e sourceEntry) string {
	if e.Kind == "dir" {
		return "0:" + e.Name
	}
	return "1:" + e.Name
}

// Retain only the next page while scanning, even for very large directories.
type sourcePage []sourceEntry

func (h sourcePage) Len() int           { return len(h) }
func (h sourcePage) Less(i, j int) bool { return entryKey(h[i]) > entryKey(h[j]) }
func (h sourcePage) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *sourcePage) Push(value any)    { *h = append(*h, value.(sourceEntry)) }
func (h *sourcePage) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func (r *Runtime) previewSourceRoot(w http.ResponseWriter, req *http.Request) (*os.Root, Source, string, bool) {
	var source Source
	if e := r.Store.Get(req.Context(), "sources", req.PathValue("id"), &source); e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			fail(w, 404, "source_not_found", "备份源不存在")
		} else {
			fail(w, 500, "source_read_failed", "无法读取备份源配置")
		}
		return nil, source, "", false
	}
	actual, e := r.ValidateSourcePath(source.Path)
	if e != nil {
		fail(w, 400, "source_path_invalid", e.Error())
		return nil, source, "", false
	}
	identity, e := MountIdentity(actual)
	if e != nil || identity != source.MountIdentity {
		fail(w, 409, "source_mount_changed", "备份源挂载身份已变化，请检查挂载后重试")
		return nil, source, "", false
	}
	rel := req.URL.Query().Get("path")
	if rel == "" {
		rel = "."
	}
	if !filepath.IsLocal(rel) || strings.ContainsRune(rel, 0) || !utf8.ValidString(rel) {
		fail(w, 400, "invalid_source_path", "请使用备份源内的相对路径")
		return nil, source, "", false
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == ".." {
			fail(w, 400, "invalid_source_path", "路径不能包含上级跳转")
			return nil, source, "", false
		}
	}
	root, e := os.OpenRoot(actual)
	if e != nil {
		fail(w, 400, "source_unreadable", "无法读取备份源")
		return nil, source, "", false
	}
	current := ""
	base, e := root.Stat(".")
	if e != nil {
		root.Close()
		fail(w, 400, "source_unreadable", "无法读取备份源")
		return nil, source, "", false
	}
	openedIdentity, e := directoryIdentity(actual, base)
	if e != nil || openedIdentity != source.MountIdentity {
		root.Close()
		fail(w, 409, "source_mount_changed", "备份源挂载身份已变化，请检查挂载后重试")
		return nil, source, "", false
	}
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		if part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, e := root.Lstat(current)
		if e != nil || info.Mode()&os.ModeSymlink != 0 || source.OneFileSystem && differentFilesystem(base, info) {
			root.Close()
			fail(w, 400, "source_entry_unavailable", "条目不可读取，或属于符号链接、其他文件系统")
			return nil, source, "", false
		}
	}
	return root, source, filepath.Clean(rel), true
}
func differentFilesystem(base, info os.FileInfo) bool {
	left, ok := base.Sys().(*syscall.Stat_t)
	right, ok2 := info.Sys().(*syscall.Stat_t)
	return ok && ok2 && left.Dev != right.Dev
}
func (r *Runtime) sourceFiles(w http.ResponseWriter, req *http.Request) {
	root, source, rel, ok := r.previewSourceRoot(w, req)
	if !ok {
		return
	}
	defer root.Close()
	f, e := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		fail(w, 400, "source_unreadable", "目录无法读取")
		return
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.IsDir() {
		fail(w, 400, "not_a_directory", "请选择目录")
		return
	}
	limit, _ := pagination(req)
	cursor := ""
	if value := req.URL.Query().Get("cursor"); value != "" {
		decoded, e := base64.RawURLEncoding.DecodeString(value)
		cursor = string(decoded)
		if e != nil || len(cursor) < 3 || len(cursor) > 1024 || cursor[:2] != "0:" && cursor[:2] != "1:" || strings.ContainsAny(cursor[2:], "/\x00") || !utf8.ValidString(cursor) {
			fail(w, 400, "invalid_cursor", "目录分页位置无效")
			return
		}
	}
	search := strings.ToLower(req.URL.Query().Get("search"))
	page := &sourcePage{}
	total, remaining := 0, 0
	for {
		entries, readErr := f.ReadDir(256)
		for _, entry := range entries {
			if !utf8.ValidString(entry.Name()) || !strings.Contains(strings.ToLower(entry.Name()), search) {
				continue
			}
			total++
			kind := "file"
			if entry.IsDir() {
				kind = "dir"
			} else if entry.Type()&os.ModeSymlink != 0 {
				kind = "symlink"
			} else if entry.Type() != 0 {
				kind = "special"
			}
			item := sourceEntry{Name: entry.Name(), Path: filepath.ToSlash(filepath.Join(rel, entry.Name())), Kind: kind}
			if entryKey(item) <= cursor {
				continue
			}
			remaining++
			if page.Len() < limit {
				heap.Push(page, item)
			} else if entryKey(item) < entryKey((*page)[0]) {
				(*page)[0] = item
				heap.Fix(page, 0)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			fail(w, 400, "source_unreadable", "读取目录失败")
			return
		}
		if req.Context().Err() != nil {
			return
		}
	}
	items := []sourceEntry(*page)
	sort.Slice(items, func(i, j int) bool { return entryKey(items[i]) < entryKey(items[j]) })
	base, _ := root.Stat(".")
	for i := range items {
		item := &items[i]
		info, e := root.Lstat(filepath.FromSlash(item.Path))
		if e != nil {
			item.Reason = "条目已变化或无法读取"
			continue
		}
		size := info.Size()
		item.Size, item.ModifiedAt, item.Mode = &size, info.ModTime(), info.Mode().String()
		item.CanOpen = info.IsDir() || info.Mode().IsRegular()
		if info.Mode()&os.ModeSymlink != 0 {
			item.Kind = "symlink"
			item.CanOpen = false
			item.LinkTarget, _ = root.Readlink(filepath.FromSlash(item.Path))
			item.Reason = "仅展示链接，不跟随目标"
		}
		if source.OneFileSystem && base != nil && differentFilesystem(base, info) {
			item.CanOpen = false
			item.Reason = "备份源策略禁止跨越文件系统"
		}
	}
	next := ""
	if remaining > len(items) && len(items) > 0 {
		next = base64.RawURLEncoding.EncodeToString([]byte(entryKey(items[len(items)-1])))
	}
	if rel == "." {
		rel = ""
	}
	writeJSON(w, 200, map[string]any{"source_id": source.ID, "path": filepath.ToSlash(rel), "items": items, "total": total, "next_cursor": next})
}
func (r *Runtime) sourceContent(w http.ResponseWriter, req *http.Request) {
	root, _, rel, ok := r.previewSourceRoot(w, req)
	if !ok {
		return
	}
	defer root.Close()
	entry, e := root.Lstat(rel)
	if e != nil || !entry.Mode().IsRegular() {
		fail(w, 400, "source_not_regular", "只支持普通文件预览")
		return
	}
	f, e := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		fail(w, 400, "source_unreadable", "文件无法读取")
		return
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		fail(w, 400, "source_not_regular", "只支持普通文件预览")
		return
	}
	const maximum = 5 << 20
	if info.Size() > maximum {
		fail(w, 413, "preview_too_large", "文件超过 5 MiB，仅显示属性")
		return
	}
	content, e := io.ReadAll(io.LimitReader(f, maximum+1))
	if e != nil {
		fail(w, 400, "source_unreadable", "读取文件失败")
		return
	}
	if len(content) > maximum {
		fail(w, 413, "preview_too_large", "文件超过 5 MiB，仅显示属性")
		return
	}
	mime := http.DetectContentType(content)
	if mime == "image/png" || mime == "image/jpeg" || mime == "image/gif" || mime == "image/webp" {
		writeJSON(w, 200, map[string]any{"kind": "image", "mime": mime, "data": base64.StdEncoding.EncodeToString(content)})
		return
	}
	if utf8.Valid(content) && !strings.ContainsRune(string(content), 0) {
		truncated := len(content) > 256<<10
		if truncated {
			content = content[:256<<10]
			for !utf8.Valid(content) {
				content = content[:len(content)-1]
			}
		}
		writeJSON(w, 200, map[string]any{"kind": "text", "text": string(content), "truncated": truncated})
		return
	}
	fail(w, 415, "preview_unsupported", "此文件格式仅支持查看属性")
}
