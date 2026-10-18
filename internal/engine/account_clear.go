package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vaultpilot/internal/process"
)

const clearConfirmation = "清空所选备份"

type AccountClearRequest struct {
	AccountIDs   []string `json:"account_ids"`
	PreviewToken string   `json:"preview_token"`
	Confirmation string   `json:"confirmation"`
}
type clearAccountItem struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Email          string `json:"email"`
	DriveID        string `json:"drive_id"`
	EstimatedBytes int64  `json:"estimated_bytes"`
}
type AccountClearPreview struct {
	VaultID             string             `json:"vault_id"`
	Path                string             `json:"path"`
	Items               []clearAccountItem `json:"items"`
	RepositoryIDs       []string           `json:"repository_ids"`
	PointIDs            []string           `json:"point_ids"`
	ProtectedPointIDs   []string           `json:"protected_point_ids"`
	PausePlanIDs        []string           `json:"pause_plan_ids"`
	CatalogAccountCount int                `json:"catalog_account_count"`
	Blocked             bool               `json:"blocked"`
	Reasons             []string           `json:"reasons"`
	PreviewToken        string             `json:"preview_token"`
	Resume              bool               `json:"resume"`
	repositories        []Repository
	plans               []Plan
	accounts            []Account
}
type clearAccountResult struct {
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	Cleared   bool   `json:"cleared"`
	Error     string `json:"error,omitempty"`
	Warning   string `json:"warning,omitempty"`
}
type accountClearResult struct {
	Accounts             []clearAccountResult `json:"accounts"`
	DeletedPointIDs      []string             `json:"deleted_point_ids"`
	RetiredRepositoryIDs []string             `json:"retired_repository_ids"`
	PausedPlanIDs        []string             `json:"paused_plan_ids"`
	CleanupPending       bool                 `json:"cleanup_pending"`
}

func (s *Service) noActiveTasks(ctx context.Context, except string) error {
	raws, err := s.rt.Store.All(ctx, "backup_runs")
	if err != nil {
		return err
	}
	for _, raw := range raws {
		var task Task
		if err = json.Unmarshal(raw, &task); err != nil {
			return err
		}
		if task.ID != except && !terminal(task.Status) {
			return fail("TASKS_ACTIVE", "存在排队或执行中的任务，请等待完成或取消后再清空")
		}
	}
	return nil
}
func (s *Service) clearMutationAllowed(ctx context.Context) error {
	s.mu.Lock()
	active := s.active["account-clear"]
	s.mu.Unlock()
	pending, err := s.rt.Store.Meta(ctx, "account_clear_pending")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if active != "" || pending != "" {
		return fail("ACCOUNT_CLEAR_PENDING", "网盘清空尚未完成，请完成清空后再修改计划或恢复点")
	}

	return nil
}
func (s *Service) buildClearPreview(ctx context.Context, ids []string, exceptTask string) (AccountClearPreview, error) {
	p := AccountClearPreview{VaultID: s.rt.VaultID(), Items: []clearAccountItem{}, RepositoryIDs: []string{}, PointIDs: []string{}, ProtectedPointIDs: []string{}, PausePlanIDs: []string{}, Reasons: []string{}}
	if s.rt.RecoveryMode() {
		return p, fail("RECOVERY_READ_ONLY", "恢复节点禁止清空网盘，请在备份写入节点操作")
	}
	if !s.rt.Keys.Unlocked() {
		return p, fail("VAULT_LOCKED", "请先解锁备份库")
	}
	if !cloudVaultID(p.VaultID) {
		return p, fail("INVALID_VAULT", "当前备份库标识无效")
	}
	p.Path = "VaultPilot/" + p.VaultID
	if len(ids) == 0 || len(ids) > 100 {
		return p, fail("INVALID_SELECTION", "请选择 1 到 100 个已验证的存储账号")
	}
	selected := map[string]bool{}
	for _, id := range ids {
		if id == "" || selected[id] {
			return p, fail("INVALID_SELECTION", "账号选择不能为空或重复")
		}
		selected[id] = true
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	if pending, _ := s.rt.Store.Meta(ctx, "account_clear_pending"); pending != "" {
		var prior []string
		if json.Unmarshal([]byte(pending), &prior) != nil {
			return p, fail("ACCOUNT_CLEAR_PENDING", "清理状态无效，请检查节点状态")
		}
		if strings.Join(prior, "\n") != strings.Join(sorted, "\n") {
			return p, fail("ACCOUNT_CLEAR_PENDING", "请重新选择上次清空的同一组账号，完成未结束的清理")
		}
		p.Resume = true
	}
	// Hash all relevant configuration so a preview cannot approve a different
	// grant, Drive, pool, repository, protected point or plan later on.
	digest := sha256.New()
	fmt.Fprintln(digest, p.VaultID)
	encoded, _ := json.Marshal(sorted)
	digest.Write(encoded)
	rawTables := map[string][]json.RawMessage{}
	for _, table := range []string{"accounts", "storage_pools", "repositories", "recovery_points", "backup_plans"} {
		raws, err := s.rt.Store.All(ctx, table)
		if err != nil {
			return p, err
		}
		rawTables[table] = raws
		fmt.Fprintln(digest, table)
		for _, raw := range raws {
			digest.Write(raw)
			digest.Write([]byte{0})
		}
	}
	p.PreviewToken = hex.EncodeToString(digest.Sum(nil))
	driveSelected := map[string]bool{}
	domain := func(region, tenant, drive string) string { return region + ":" + tenant + ":" + drive }
	seenDrives := map[string]bool{}
	for _, raw := range rawTables["accounts"] {
		var a Account
		var identity struct {
			Email string `json:"account_email"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return p, err
		}
		json.Unmarshal(raw, &identity)
		if selected[a.ID] {
			if !a.Verified || a.DriveID == "" || (a.Backend != "onedrive" && a.Backend != "local") {
				return p, fail("ACCOUNT_UNVERIFIED", "所选账号必须先完成访问验证")
			}
			key := domain(a.Region, a.TenantID, a.DriveID)
			if driveSelected[key] {
				return p, fail("DUPLICATE_DRIVE", "同一网盘不能重复选择")
			}
			driveSelected[key] = true
			p.Items = append(p.Items, clearAccountItem{a.ID, a.Name, identity.Email, a.DriveID, a.ProjectUsed})
		}
		if a.Verified && a.DriveID != "" {
			key := domain(a.Region, a.TenantID, a.DriveID)
			if seenDrives[key] {
				return p, fail("DUPLICATE_DRIVE", "已登记的账号存在重复网盘，请先处理重复账号配置")
			}
			if !seenDrives[key] {
				p.accounts = append(p.accounts, a)
				seenDrives[key] = true
			}
		}
	}
	if len(p.Items) != len(selected) {
		return p, fail("ACCOUNT_MISSING", "所选账号不存在")
	}
	p.CatalogAccountCount = len(p.accounts)
	lost := map[string]bool{}
	plansToPause := map[string]bool{}
	for _, raw := range rawTables["repositories"] {
		var repo Repository
		if err := json.Unmarshal(raw, &repo); err != nil {
			return p, err
		}
		touched := selected[repo.AccountID]
		for i, loc := range repo.Locations {
			if driveSelected[domain(loc.Region, loc.TenantID, loc.DriveID)] || selected[loc.AccountID] {
				if loc.Status != "deleted" {
					touched = true
				}
				repo.Locations[i].Verified = false
				repo.Locations[i].Status = "deleted"
			}
		}
		if !touched {
			continue
		}
		plansToPause[repo.PlanID] = true
		if selected[repo.AccountID] && repo.Status != "retired" {
			replacement := -1
			for i, loc := range repo.Locations {
				if !loc.Verified || loc.Status == "deleted" {
					continue
				}
				for _, a := range p.accounts {
					if domain(a.Region, a.TenantID, a.DriveID) == domain(loc.Region, loc.TenantID, loc.DriveID) {
						replacement = i
						repo.AccountID = a.ID
						break
					}
				}
			}
			if replacement >= 0 {
				repo.Path = repo.Locations[replacement].Path
			} else {
				repo.Status = "retired"
				lost[repo.ID] = true
				p.RepositoryIDs = append(p.RepositoryIDs, repo.ID)
			}
		}
		p.repositories = append(p.repositories, repo)
	}
	for _, raw := range rawTables["recovery_points"] {
		var point Point
		if err := json.Unmarshal(raw, &point); err != nil {
			return p, err
		}
		if point.DeletedAt != "" {
			continue
		}
		affected := false
		for _, snap := range point.Snapshots {
			if !snap.Empty && lost[snap.RepositoryID] {
				affected = true
			}
		}
		if affected {
			p.PointIDs = append(p.PointIDs, point.ID)
			if point.Protected {
				p.ProtectedPointIDs = append(p.ProtectedPointIDs, point.ID)
			}
		}
	}
	for _, raw := range rawTables["storage_pools"] {
		var pool Pool
		if err := json.Unmarshal(raw, &pool); err != nil {
			return p, err
		}
		for _, id := range pool.AccountIDs {
			if selected[id] {
				plansToPause["pool:"+pool.ID] = true
			}
		}
	}
	for _, raw := range rawTables["backup_plans"] {
		var plan Plan
		if err := json.Unmarshal(raw, &plan); err != nil {
			return p, err
		}
		if plan.Enabled && (plansToPause[plan.ID] || plansToPause["pool:"+plan.PoolID]) {
			p.PausePlanIDs = append(p.PausePlanIDs, plan.ID)
			plan.Enabled = false
			p.plans = append(p.plans, plan)
		}
	}
	if len(p.ProtectedPointIDs) > 0 {
		p.Reasons = append(p.Reasons, "涉及受保护恢复点，请先在恢复中心取消保护后重新预览")
	}
	if err := s.noActiveTasks(ctx, exceptTask); err != nil {
		p.Reasons = append(p.Reasons, err.Error())
	}
	p.Blocked = len(p.Reasons) > 0
	return p, nil
}
func (s *Service) clearPreviewHTTP(w http.ResponseWriter, r *http.Request) {
	var req AccountClearRequest
	if err := decode(r, &req); err != nil {
		problem(w, err)
		return
	}
	release, err := s.rt.LockAccountConfigurationContext(r.Context())
	if err != nil {
		return
	}
	defer release()
	if err := s.catalogMu.LockContext(r.Context()); err != nil {
		return
	}
	defer s.catalogMu.Unlock()
	p, err := s.buildClearPreview(r.Context(), req.AccountIDs, "")
	if err != nil {
		problem(w, err)
		return
	}
	write(w, 200, p)
}
func (s *Service) clearSubmitHTTP(w http.ResponseWriter, r *http.Request) {
	var req AccountClearRequest
	if err := decode(r, &req); err != nil {
		problem(w, err)
		return
	}
	if req.Confirmation != clearConfirmation || len(req.PreviewToken) != 64 {
		problem(w, fail("CONFIRM_REQUIRED", "请先预览并输入“清空所选备份”确认不可撤销的删除"))
		return
	}
	if s.rt.RecoveryMode() || !s.rt.Keys.Unlocked() {
		problem(w, fail("CLEAR_NOT_ALLOWED", "请在已解锁的备份写入节点操作"))
		return
	}
	t, err := s.enqueue("account-clear", "", r.Header.Get("Idempotency-Key"), req)
	if err != nil {
		problem(w, err)
		return
	}
	write(w, 202, t)
}

// Persist retirement before remote deletion. A durable pending marker prevents
// backups/discovery from reusing old state after crashes or partial failures.
func (s *Service) retireForClear(ctx context.Context, p AccountClearPreview, ids []string) error {
	tx, err := s.rt.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, repo := range p.repositories {
		if err = saveCatalogEntity(ctx, tx, "repositories", repo.ID, repo); err != nil {
			return err
		}
		if repo.Status == "retired" {
			if _, err = tx.ExecContext(ctx, "DELETE FROM assignments WHERE repo_id=?", repo.ID); err != nil {
				return err
			}
		}
	}
	for _, id := range p.PointIDs {
		var point Point
		var raw []byte
		if err = tx.QueryRowContext(ctx, "SELECT data FROM recovery_points WHERE id=?", id).Scan(&raw); err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &point); err != nil {
			return err
		}
		point.DeletedAt = now()
		point.Status = "deleted"
		if err = saveCatalogEntity(ctx, tx, "recovery_points", id, point); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM file_entries WHERE run_id=?", point.RunID); err != nil {
			return err
		}
	}
	for _, plan := range p.plans {
		if err = saveCatalogEntity(ctx, tx, "backup_plans", plan.ID, plan); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,'0') ON CONFLICT(key) DO UPDATE SET value=excluded.value", "pending:"+plan.ID); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, "DELETE FROM capacity_reservations WHERE account_id=?", id); err != nil {
			return err
		}
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	raw, _ := json.Marshal(sorted)
	if _, err = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES('account_clear_pending',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(raw)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM metadata WHERE key='catalog_discovered' OR key LIKE 'catalog_manifest:%'"); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) clearAccounts(ctx context.Context, t *Task) error {
	var req AccountClearRequest
	if err := json.Unmarshal(t.Request, &req); err != nil {
		return err
	}
	if req.Confirmation != clearConfirmation {
		return fail("CONFIRM_REQUIRED", "清空备份需要明确确认")
	}
	if err := s.configurationMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.configurationMu.Unlock()
	release, err := s.rt.LockAccountConfigurationContext(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := s.recoveryIndex.LockContext(ctx); err != nil {
		return err
	}
	defer s.recoveryIndex.Unlock()
	if err := s.catalogMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.catalogMu.Unlock()
	p, err := s.buildClearPreview(ctx, req.AccountIDs, t.ID)
	if err != nil {
		return err
	}
	if p.Blocked {
		return fail("ACCOUNT_CLEAR_BLOCKED", strings.Join(p.Reasons, "；"))
	}
	if req.PreviewToken != p.PreviewToken {
		return fail("VERSION_CONFLICT", "预览后账号或备份记录已变化，请重新预览确认")
	}
	for _, a := range p.accounts {
		if err = validateClearLocal(a, p.Path); err != nil {
			return err
		}
	}
	s.stage(t, "recording_deletions")
	if err = s.retireForClear(ctx, p, req.AccountIDs); err != nil {
		return err
	}
	defer s.reloadSchedules()
	result := accountClearResult{Accounts: []clearAccountResult{}, DeletedPointIDs: p.PointIDs, RetiredRepositoryIDs: p.RepositoryIDs, PausedPlanIDs: p.PausePlanIDs, CleanupPending: true}
	t.Result = result
	s.save(t)
	// Require a fresh authenticated retirement certificate on EVERY registered
	// verified Drive, including disabled ones, before deleting any data. Keeping
	// that certificate avoids old cloud indexes resurrecting cleared backups.
	pool := Pool{CatalogReplicas: len(p.accounts), includeDisabled: true}
	for _, a := range p.accounts {
		pool.AccountIDs = append(pool.AccountIDs, a.ID)
	}
	s.stage(t, "committing_clear_index")
	if err = s.commit(ctx, t, pool, nil, nil); err != nil {
		return err
	}
	commitID, err := s.rt.Store.Meta(ctx, "catalog_last")
	if err != nil || !cloudVaultID(commitID) {
		return fail("CATALOG_COMMIT_INCOMPLETE", "清理目录提交未确认，未删除云端数据")
	}
	s.stage(t, "clearing_accounts")
	selected := map[string]bool{}
	for _, id := range req.AccountIDs {
		selected[id] = true
	}
	failed := false
	for _, a := range p.accounts {
		if !selected[a.ID] {
			continue
		}
		item := clearAccountResult{AccountID: a.ID, Name: a.Name}
		if ctx.Err() != nil {
			item.Error = "任务已取消，剩余账号需重新清空"
		} else if err = s.clearAccountData(ctx, a, p.Path, commitID, t); err != nil {
			item.Error = err.Error()
		} else {
			item.Cleared = true
			if err = s.refreshAccount(ctx, &a); err != nil {
				item.Warning = "数据已清空，但配额刷新失败，请稍后手动刷新"
			}
		}
		if !item.Cleared {
			failed = true
		}
		result.Accounts = append(result.Accounts, item)
		t.Result = result
		s.save(t)
		s.log(t, fmt.Sprintf("%s：清空完成=%t %s", a.Name, item.Cleared, item.Error))
	}
	if failed {
		return fail("ACCOUNT_CLEAR_PARTIAL", "部分账号清空未完成；已完成的删除不可撤销，请选择同一组账号重新预览并执行")
	}
	if err = s.rt.Store.SetMeta(ctx, "account_clear_pending", ""); err != nil {
		return err
	}
	result.CleanupPending = false
	t.Result = result
	return s.rt.Store.Audit(ctx, "accounts.clear", t.ID)
}
func validateClearLocal(a Account, base string) error {
	if a.Backend != "local" {
		return nil
	}
	root, err := os.OpenRoot(a.LocalPath)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, path := range []string{"VaultPilot", base, base + "/repositories", base + "/catalog"} {
		info, e := root.Lstat(path)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fail("UNSAFE_CLEAR_PATH", "备份目录必须是真实目录，拒绝清理符号链接或非目录入口")
		}
	}
	return nil
}
func (s *Service) clearAccountData(ctx context.Context, a Account, base, keepCommit string, t *Task) error {
	// Construct paths only from the server's validated vault identity. Never
	// accept a client-supplied remote, directory or rclone filter.
	if !cloudVaultID(s.rt.VaultID()) || base != "VaultPilot/"+s.rt.VaultID() || !cloudVaultID(keepCommit) {
		return fail("UNSAFE_CLEAR_PATH", "清空范围无效")
	}
	if a.Backend == "local" {
		if err := validateClearLocal(a, base); err != nil {
			return err
		}
		root, err := os.OpenRoot(a.LocalPath)
		if err != nil {
			return err
		}
		defer root.Close()
		if err = root.RemoveAll(filepath.FromSlash(base + "/repositories")); err != nil {
			return err
		}
		dir, err := root.Open(base + "/catalog")
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		entries, err := dir.ReadDir(-1)
		dir.Close()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if obsoleteClearCatalog(entry.Name(), keepCommit) {
				if err = root.Remove(base + "/catalog/" + entry.Name()); err != nil {
					return err
				}
			}
		}
		return nil
	}
	_, err := s.rcloneRun(ctx, []Account{a}, func(line string) { s.log(t, line) }, "purge", s.remoteFile(a, base+"/repositories"))
	_ = s.rt.SyncRcloneCredentials()
	if err != nil && process.ExitCode(err) != 3 {
		return err
	}
	args := clearCatalogDeleteArguments(s.remoteFile(a, base+"/catalog"), keepCommit)
	_, err = s.rcloneRun(ctx, []Account{a}, func(line string) { s.log(t, line) }, args...)
	_ = s.rt.SyncRcloneCredentials()
	return err
}
func obsoleteClearCatalog(name, keep string) bool {
	for _, ext := range []string{".vpc", ".ok", ".prepare"} {
		if strings.HasSuffix(name, ext) {
			id := strings.TrimSuffix(name, ext)
			return cloudVaultID(id) && id != keep
		}
	}
	return false
}

func clearCatalogDeleteArguments(remote, keep string) []string {
	return []string{"delete", remote, "--max-depth", "1", "--filter", "- /" + keep + ".*", "--filter", "+ /*.vpc", "--filter", "+ /*.ok", "--filter", "+ /*.prepare", "--filter", "- *"}
}
