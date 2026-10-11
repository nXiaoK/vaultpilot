package engine

import (
	"context"
	"path/filepath"
)

const (
	restoreLayoutAuto     = "auto"
	restoreLayoutDirect   = "direct"
	restoreLayoutBySource = "by_source"
)

type RestoreOutputRoot struct {
	SourceID string `json:"source_id"`
	Path     string `json:"path"`
}

// Resolve the entire selection before considering account availability. A
// missing account must not change output paths when partial restore is enabled.
func (s *Service) resolveRestoreLayout(ctx context.Context, req RestoreRequest, where string, args []any) (string, []RestoreOutputRoot, error) {
	layout := req.Layout
	if layout == "" {
		layout = restoreLayoutAuto
	}
	if layout != restoreLayoutAuto && layout != restoreLayoutDirect && layout != restoreLayoutBySource {
		return "", nil, fail("INVALID_RESTORE_LAYOUT", "恢复目录布局无效，请选择自动、直接恢复或按备份源分目录")
	}
	rows, err := s.rt.Store.DB.QueryContext(ctx, "SELECT DISTINCT source_id FROM file_entries WHERE "+where+" ORDER BY source_id", args...)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	sources := []string{}
	for rows.Next() {
		var source string
		if err = rows.Scan(&source); err != nil {
			return "", nil, err
		}
		if !safeID(source) {
			return "", nil, fail("RESTORE_PATH_INVALID", "恢复清单的备份源标识无效")
		}
		sources = append(sources, source)
	}
	if err = rows.Err(); err != nil {
		return "", nil, err
	}
	if layout == restoreLayoutAuto {
		layout = restoreLayoutBySource
		if len(sources) == 1 {
			layout = restoreLayoutDirect
		}
	}
	if layout == restoreLayoutDirect && len(sources) > 1 {
		return "", nil, fail("RESTORE_LAYOUT_REQUIRES_SINGLE_SOURCE", "直接恢复只允许选择一个备份源；请选择单个源，或改用自动/按备份源分目录")
	}
	roots := []RestoreOutputRoot{}
	for _, source := range sources {
		path := filepath.Clean(req.Target)
		if layout == restoreLayoutBySource {
			path = filepath.Join(path, source)
		}
		roots = append(roots, RestoreOutputRoot{SourceID: source, Path: path})
	}
	return layout, roots, nil
}

// Use this same mapping for preflight, writes, and directory metadata. Paths
// remain relative to the logical source; selecting a subtree does not flatten it.
func restoreOutputPath(e Entry, layout string) (string, error) {
	if !safeID(e.SourceID) || !safeLogical(e.Path) || (e.Path == "" && e.Kind != "dir") {
		return "", fail("RESTORE_PATH_INVALID", "恢复清单路径无效")
	}
	switch layout {
	case restoreLayoutDirect:
		if e.Path == "" {
			return ".", nil
		}
		return filepath.FromSlash(e.Path), nil
	case restoreLayoutBySource:
		return filepath.Join(e.SourceID, filepath.FromSlash(e.Path)), nil
	default:
		return "", fail("INVALID_RESTORE_LAYOUT", "恢复目录布局尚未确定")
	}
}
