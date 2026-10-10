package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"vaultpilot/internal/store"
)

type Account struct {
	ID                      string `json:"id"`
	Version                 int    `json:"version"`
	Name                    string `json:"name"`
	Backend                 string `json:"backend"`
	Remote                  string `json:"remote"`
	DriveID                 string `json:"drive_id"`
	DriveName               string `json:"drive_name"`
	DriveType               string `json:"drive_type"`
	Region                  string `json:"region"`
	TenantID                string `json:"tenant_id"`
	QuotaDomain             string `json:"quota_domain"`
	Status                  string `json:"status"`
	Enabled                 bool   `json:"enabled"`
	BudgetBytes             int64  `json:"budget_bytes"`
	CloudReserveBytes       int64  `json:"cloud_reserve_bytes"`
	MaintenanceReserveBytes int64  `json:"maintenance_reserve_bytes"`
	TotalBytes              int64  `json:"total_bytes"`
	UsedBytes               int64  `json:"used_bytes"`
	FreeBytes               int64  `json:"free_bytes"`
	ProjectUsedBytes        int64  `json:"project_used_bytes"`
	QuotaAt                 string `json:"quota_at"`
	Verified                bool   `json:"verified"`
	LocalPath               string `json:"local_path,omitempty"`
}
type Pool struct {
	ID               string   `json:"id"`
	Version          int      `json:"version"`
	Name             string   `json:"name"`
	AccountIDs       []string `json:"account_ids"`
	CatalogReplicas  int      `json:"catalog_replicas"`
	ShardTargetBytes int64    `json:"shard_target_bytes"`
}
type Source struct {
	ID            string   `json:"id"`
	Version       int      `json:"version"`
	Name          string   `json:"name"`
	Path          string   `json:"path"`
	MountIdentity string   `json:"mount_identity"`
	Excludes      []string `json:"excludes"`
	IncludeHidden bool     `json:"include_hidden"`
	MaxFileBytes  int64    `json:"max_file_bytes"`
	OneFileSystem bool     `json:"one_file_system"`
}

func pagination(req *http.Request) (int, int) {
	limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(req.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
func (r *Runtime) list(w http.ResponseWriter, req *http.Request, table string) {
	l, o := pagination(req)
	items, total, e := r.Store.List(req.Context(), table, l, o)
	if e != nil {
		fail(w, 500, "database_error", "无法读取列表")
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total})
}
func (r *Runtime) listAccounts(w http.ResponseWriter, req *http.Request) { r.list(w, req, "accounts") }
func (r *Runtime) listPools(w http.ResponseWriter, req *http.Request) {
	r.list(w, req, "storage_pools")
}
func (r *Runtime) listSources(w http.ResponseWriter, req *http.Request) { r.list(w, req, "sources") }
func saved(w http.ResponseWriter, status int, e error, v any) {
	if errors.Is(e, store.ErrConflict) {
		fail(w, 409, "version_conflict", "配置已被修改，请重新加载")
		return
	}
	if e != nil {
		fail(w, 500, "database_error", "保存配置失败")
		return
	}
	writeJSON(w, status, v)
}
func validBudget(a Account) error {
	if a.BudgetBytes < 1 || a.CloudReserveBytes < 0 || a.MaintenanceReserveBytes < 0 || a.MaintenanceReserveBytes >= a.BudgetBytes {
		return errors.New("项目预算必须大于零且大于维护预留；预留不能为负数")
	}
	return nil
}
func (r *Runtime) createAccount(w http.ResponseWriter, req *http.Request) {
	var a Account
	if !decode(w, req, &a) {
		return
	}
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" || len(a.Name) > 200 {
		fail(w, 400, "invalid_name", "账号名称不能为空且不超过 200 字符")
		return
	}
	if a.Backend == "" {
		a.Backend = "onedrive"
	}
	if a.Backend != "onedrive" && a.Backend != "local" {
		fail(w, 400, "invalid_backend", "仅支持 OneDrive 或显式启用的本地联调")
		return
	}
	if a.Backend == "local" && !r.Config.AllowLocal {
		fail(w, 403, "local_disabled", "本地联调未启用")
		return
	}
	if a.BudgetBytes == 0 && a.Backend == "onedrive" {
		a.BudgetBytes = 1 << 40
	}
	if e := validBudget(a); e != nil {
		fail(w, 400, "invalid_budget", e.Error())
		return
	}
	a.ID = newID()
	a.Version = 0
	a.Remote = "vp_" + a.ID
	a.Enabled = true
	a.Verified = false
	a.Status = "awaiting_authorization"
	a.DriveID = ""
	a.DriveName = ""
	a.DriveType = ""
	a.Region = "global"
	a.TenantID = r.Config.OAuthTenant
	a.QuotaDomain = ""
	a.TotalBytes = 0
	a.UsedBytes = 0
	a.FreeBytes = 0
	a.ProjectUsedBytes = 0
	a.QuotaAt = ""
	if a.Backend == "local" {
		if e := r.verifyLocal(req.Context(), &a); e != nil {
			fail(w, 400, "local_verification_failed", e.Error())
			return
		}
	} else {
		a.LocalPath = ""
	}
	v, e := r.Store.Save(req.Context(), "accounts", a.ID, 0, a)
	a.Version = v
	saved(w, 201, e, a)
	if e == nil {
		_ = r.Store.Audit(req.Context(), "account.create", a.ID)
	}
}
func (r *Runtime) updateAccount(w http.ResponseWriter, req *http.Request) {
	var old Account
	if e := r.Store.Get(req.Context(), "accounts", req.PathValue("id"), &old); e != nil {
		fail(w, 404, "account_not_found", "账号不存在")
		return
	}
	var input struct {
		Name                    string `json:"name"`
		Version                 int    `json:"version"`
		Enabled                 *bool  `json:"enabled"`
		BudgetBytes             int64  `json:"budget_bytes"`
		CloudReserveBytes       int64  `json:"cloud_reserve_bytes"`
		MaintenanceReserveBytes int64  `json:"maintenance_reserve_bytes"`
	}
	if !decode(w, req, &input) {
		return
	}
	if input.Version <= 0 {
		fail(w, 400, "version_required", "修改必须提供当前版本")
		return
	}
	old.Name = strings.TrimSpace(input.Name)
	if old.Name == "" {
		fail(w, 400, "invalid_name", "账号名称不能为空")
		return
	}
	old.BudgetBytes = input.BudgetBytes
	old.CloudReserveBytes = input.CloudReserveBytes
	old.MaintenanceReserveBytes = input.MaintenanceReserveBytes
	if e := validBudget(old); e != nil {
		fail(w, 400, "invalid_budget", e.Error())
		return
	}
	if input.Enabled != nil {
		old.Enabled = *input.Enabled
	}
	v, e := r.Store.Save(req.Context(), "accounts", old.ID, input.Version, old)
	old.Version = v
	saved(w, 200, e, old)
	if e == nil {
		_ = r.Store.Audit(req.Context(), "account.update", old.ID)
	}
}
func (r *Runtime) deleteAccount(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	for _, table := range []string{"repositories", "storage_pools"} {
		rows, e := r.Store.All(req.Context(), table)
		if e != nil {
			fail(w, 500, "database_error", "无法检查账号依赖")
			return
		}
		for _, raw := range rows {
			var obj map[string]any
			_ = json.Unmarshal(raw, &obj)
			dependent := obj["account_id"] == id
			if ids, ok := obj["account_ids"].([]any); ok {
				for _, v := range ids {
					if v == id {
						dependent = true
					}
				}
			}
			if loc, ok := obj["locations"].([]any); ok {
				for _, v := range loc {
					m, _ := v.(map[string]any)
					if m["account_id"] == id {
						dependent = true
					}
				}
			}
			if dependent {
				fail(w, 409, "account_in_use", "账号仍被仓库或存储池引用，请先停用并处理依赖")
				return
			}
		}
	}
	if e := r.Store.Delete(req.Context(), "accounts", id); e != nil {
		fail(w, 500, "database_error", "删除账号失败")
		return
	}
	_ = r.setMeta("credential:"+id, "")
	_ = r.Store.Audit(req.Context(), "account.delete", id)
	w.WriteHeader(204)
}
func canonicalDir(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", errors.New("目录路径无效")
	}
	abs, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	actual, e := filepath.EvalSymlinks(abs)
	if e != nil {
		return "", errors.New("目录不存在或无法访问")
	}
	info, e := os.Stat(actual)
	if e != nil || !info.IsDir() {
		return "", errors.New("必须选择目录")
	}
	return actual, nil
}
func within(path, root string) bool {
	rel, e := filepath.Rel(root, path)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func overlap(a, b string) bool { return within(a, b) || within(b, a) }
func (r *Runtime) verifyLocal(ctx context.Context, a *Account) error {
	path, e := canonicalDir(a.LocalPath)
	if e != nil {
		return e
	}
	for _, blocked := range append(append([]string{r.Config.StateDir, r.Config.CacheDir}, r.Config.SourceRoots...), r.Config.RestoreRoots...) {
		b, e := canonicalDir(blocked)
		if e == nil && overlap(path, b) {
			return errors.New("本地联调仓库不能与源、状态、缓存或恢复目录重叠")
		}
	}
	all, e := r.Store.All(ctx, "accounts")
	if e != nil {
		return e
	}
	for _, raw := range all {
		var other Account
		_ = json.Unmarshal(raw, &other)
		if other.ID != a.ID && other.Backend == "local" && overlap(path, other.LocalPath) {
			return errors.New("本地仓库目录与现有账号重叠")
		}
	}
	test := filepath.Join(path, ".vaultpilot-verify-"+newID())
	content := randomBytes(32)
	if e = os.WriteFile(test, content, 0600); e != nil {
		return errors.New("本地目录无法写入")
	}
	defer os.Remove(test)
	read, e := os.ReadFile(test)
	if e != nil || string(read) != string(content) {
		return errors.New("本地目录读回验证失败")
	}
	a.LocalPath = path
	a.DriveID = "local:" + path
	a.DriveName = filepath.Base(path)
	a.DriveType = "local"
	a.Region = "local"
	a.TenantID = ""
	a.QuotaDomain = a.DriveID
	a.Status = "ready"
	a.Verified = true
	return r.localQuota(a)
}
func (r *Runtime) localQuota(a *Account) error {
	var fs syscall.Statfs_t
	if e := syscall.Statfs(a.LocalPath, &fs); e != nil {
		return errors.New("无法查询本地磁盘容量")
	}
	a.TotalBytes = int64(fs.Blocks) * int64(fs.Bsize)
	a.FreeBytes = int64(fs.Bavail) * int64(fs.Bsize)
	a.UsedBytes = a.TotalBytes - int64(fs.Bfree)*int64(fs.Bsize)
	var size int64
	root := filepath.Join(a.LocalPath, "VaultPilot", r.VaultID())
	e := filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if errors.Is(e, os.ErrNotExist) && path == root {
			return nil
		}
		if e != nil {
			return e
		}
		if d.Type().IsRegular() {
			info, e := d.Info()
			if e != nil {
				return e
			}
			size += info.Size()
		}
		return nil
	})
	if e != nil {
		return errors.New("无法核对项目占用")
	}
	a.ProjectUsedBytes = size
	a.QuotaAt = time.Now().UTC().Format(time.RFC3339)
	return nil
}
func (r *Runtime) savePool(w http.ResponseWriter, req *http.Request) {
	var p Pool
	if !decode(w, req, &p) {
		return
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.AccountIDs) == 0 {
		fail(w, 400, "invalid_pool", "请输入池名称并选择已验证账号")
		return
	}
	if p.CatalogReplicas == 0 {
		p.CatalogReplicas = 2
	}
	if p.CatalogReplicas < 1 || p.CatalogReplicas > 10 {
		fail(w, 400, "invalid_replicas", "控制目录副本数量应在 1 至 10 之间")
		return
	}
	if p.ShardTargetBytes == 0 {
		p.ShardTargetBytes = 100 << 30
	}
	if p.ShardTargetBytes < 1<<20 {
		fail(w, 400, "invalid_shard_target", "分片目标至少 1 MiB")
		return
	}
	seen := map[string]bool{}
	for _, id := range p.AccountIDs {
		var a Account
		if e := r.Store.Get(req.Context(), "accounts", id, &a); e != nil || !a.Verified || !a.Enabled || a.Status != "ready" {
			fail(w, 400, "account_unavailable", "存储池只能包含已验证且启用的账号")
			return
		}
		domain := a.QuotaDomain
		if domain == "" {
			fail(w, 400, "quota_domain_missing", "账号配额域不明确，请重新验证")
			return
		}
		if seen[domain] {
			fail(w, 409, "duplicate_quota_domain", "不能重复计算同一 Drive 或配额域容量")
			return
		}
		seen[domain] = true
	}
	if p.CatalogReplicas > len(seen) {
		fail(w, 400, "replicas_unavailable", "不同 Drive 数量不足以满足控制目录副本要求；默认要求两个")
		return
	}
	id := req.PathValue("id")
	expected := 0
	status := 201
	if id == "" {
		id = newID()
	} else {
		expected = p.Version
		status = 200
		if expected <= 0 {
			fail(w, 400, "version_required", "修改必须提供版本")
			return
		}
	}
	p.ID = id
	v, e := r.Store.Save(req.Context(), "storage_pools", id, expected, p)
	p.Version = v
	saved(w, status, e, p)
	if e == nil {
		_ = r.Store.Audit(req.Context(), "pool.save", id)
	}
}
func (r *Runtime) deletePool(w http.ResponseWriter, req *http.Request) {
	r.deleteResource(w, req, "storage_pools", "pool_id")
}
func (r *Runtime) deleteSource(w http.ResponseWriter, req *http.Request) {
	r.deleteResource(w, req, "sources", "source_ids")
}
func (r *Runtime) deleteResource(w http.ResponseWriter, req *http.Request, table, field string) {
	id := req.PathValue("id")
	plans, e := r.Store.All(req.Context(), "backup_plans")
	if e != nil {
		fail(w, 500, "database_error", "无法检查依赖")
		return
	}
	for _, raw := range plans {
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		if obj[field] == id {
			fail(w, 409, "resource_in_use", "配置仍被计划引用")
			return
		}
		if ids, ok := obj[field].([]any); ok {
			for _, v := range ids {
				if v == id {
					fail(w, 409, "resource_in_use", "配置仍被计划引用")
					return
				}
			}
		}
	}
	if e = r.Store.Delete(req.Context(), table, id); e != nil {
		fail(w, 500, "database_error", "删除失败")
		return
	}
	_ = r.Store.Audit(req.Context(), table+".delete", id)
	w.WriteHeader(204)
}
func describeError(e error) string {
	if errors.Is(e, sql.ErrNoRows) {
		return "记录不存在"
	}
	return fmt.Sprint(e)
}
