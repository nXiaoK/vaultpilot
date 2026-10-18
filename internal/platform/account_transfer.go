package platform

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const maxAccountTransfer = 1 << 20
const accountTransferAAD = "vaultpilot-accounts-v1"

var transferDrivePattern = regexp.MustCompile(`^[a-zA-Z0-9!_-]{1,200}$`)

type transferEntry struct {
	Account    Account           `json:"account"`
	Credential accountCredential `json:"credential"`
}
type transferPayload struct {
	Entries []transferEntry `json:"entries"`
}
type transferRequest struct {
	Password   string      `json:"password"`
	AccountIDs []string    `json:"account_ids"`
	DriveIDs   []string    `json:"drive_ids"`
	Package    RecoveryKit `json:"package"`
}

// Only portable settings cross nodes. Status, IDs, paths and measured capacity
// are reconstructed locally rather than trusted from the exporting node.
func portableAccount(a Account) Account {
	return Account{Name: a.Name, Backend: "onedrive", DriveID: a.DriveID, DriveName: a.DriveName, DriveType: "personal", Region: "global", TenantID: "consumers", AccountEmail: a.AccountEmail, AccountDisplayName: a.AccountDisplayName, Enabled: a.Enabled, BudgetBytes: a.BudgetBytes, CloudReserveBytes: a.CloudReserveBytes, MaintenanceReserveBytes: a.MaintenanceReserveBytes}
}
func validTransferEntry(entry transferEntry) error {
	a, c := entry.Account, entry.Credential
	if a.Backend != "onedrive" || a.DriveType != "personal" || a.Region != "global" || !transferDrivePattern.MatchString(a.DriveID) || strings.TrimSpace(a.Name) == "" || len(a.Name) > 200 || len(a.AccountEmail) > 320 || len(a.AccountDisplayName) > 512 || len(a.DriveName) > 512 {
		return errors.New("账号包包含不支持的账号或 Drive")
	}
	if err := validBudget(a); err != nil {
		return err
	}
	if c.Mode != "custom" && c.Mode != "rclone" {
		return errors.New("账号授权类型无效")
	}
	if c.Mode == "custom" && c.ClientID == "" {
		return errors.New("自定义授权缺少客户端 ID")
	}
	for _, value := range []string{c.ClientID, c.ClientSecret, c.Tenant, c.Scopes} {
		if len(value) > 65536 || strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("授权设置格式无效")
		}
	}
	token, _ := json.Marshal(c.Token)
	values := map[string]string{"type": "onedrive", "region": "global", "token": string(token), "client_id": c.ClientID, "tenant": c.Tenant, "access_scopes": c.Scopes}
	if c.Mode == "rclone" {
		values["client_secret"] = c.ClientSecret
	}
	_, err := validateRcloneEntry(a.Name, values)
	return err
}
func protectAccounts(p transferPayload, password string) (RecoveryKit, error) {
	kit := RecoveryKit{Format: "vaultpilot-accounts", Version: 1, KDF: "argon2id", Time: 3, Memory: 64 * 1024, Parallelism: 2}
	salt := randomBytes(16)
	kit.Salt = base64.StdEncoding.EncodeToString(salt)
	plain, err := json.Marshal(p)
	if err != nil {
		return kit, err
	}
	defer clear(plain)
	if len(plain) > maxAccountTransfer {
		return kit, errors.New("账号包超过 1 MiB，请分批导出")
	}
	key := deriveKey([]byte(password), salt, 3, 64*1024, 2)
	defer clear(key)
	box, err := encrypt(key, plain, []byte(accountTransferAAD))
	kit.Ciphertext = base64.StdEncoding.EncodeToString(box)
	return kit, err
}
func openAccounts(kit RecoveryKit, password string) (transferPayload, error) {
	var p transferPayload
	bad := errors.New("账号包格式无效、密码错误或文件已损坏")
	if kit.Format != "vaultpilot-accounts" || kit.Version != 1 || kit.KDF != "argon2id" || kit.Time != 3 || kit.Memory != 64*1024 || kit.Parallelism != 2 || len(kit.Ciphertext) > base64.StdEncoding.EncodedLen(maxAccountTransfer+32) {
		return p, bad
	}
	salt, err := base64.StdEncoding.DecodeString(kit.Salt)
	if err != nil || len(salt) != 16 {
		return p, bad
	}
	box, err := base64.StdEncoding.DecodeString(kit.Ciphertext)
	if err != nil || len(box) < 28 || len(box) > maxAccountTransfer+32 {
		return p, bad
	}
	key := deriveKey([]byte(password), salt, 3, 64*1024, 2)
	defer clear(key)
	plain, err := decrypt(key, box, []byte(accountTransferAAD))
	if err != nil {
		return p, bad
	}
	defer clear(plain)
	if json.Unmarshal(plain, &p) != nil || len(p.Entries) < 1 || len(p.Entries) > 50 {
		return transferPayload{}, bad
	}
	seen := map[string]bool{}
	for _, entry := range p.Entries {
		if seen[entry.Account.DriveID] {
			return transferPayload{}, bad
		}
		seen[entry.Account.DriveID] = true
		if err := validTransferEntry(entry); err != nil {
			return transferPayload{}, err
		}
	}
	return p, nil
}
func (r *Runtime) decodeTransfer(w http.ResponseWriter, req *http.Request, v *transferRequest, limit int64) bool {
	if !decodeLimit(w, req, v, limit) {
		return false
	}
	if len(v.Password) < 12 || len(v.Password) > 1024 {
		fail(w, 400, "weak_credentials", "账号包保护密码至少 12 个字符，最多 1024 个字节")
		return false
	}
	if !r.ConsumeSecretAttempt(req, "transfer") {
		w.Header().Set("Retry-After", "900")
		fail(w, 429, "rate_limited", "密码操作过于频繁，请稍后重试")
		return false
	}
	return true
}
func (r *Runtime) exportAccounts(w http.ResponseWriter, req *http.Request) {
	var v transferRequest
	if !r.decodeTransfer(w, req, &v, maxAuthRequestBytes) {
		return
	}
	if len(v.AccountIDs) < 1 || len(v.AccountIDs) > 50 {
		fail(w, 400, "account_selection_required", "请选择 1 至 50 个已验证的 OneDrive 账号")
		return
	}
	r.accountTransferMu.Lock()
	defer r.accountTransferMu.Unlock()
	var p transferPayload
	// Lock ordering matches credential installation/deletion. Release before KDF.
	r.accountMu.Lock()
	r.credentialsMu.Lock()
	err := func() error {
		seen := map[string]bool{}
		for _, id := range v.AccountIDs {
			var a Account
			if seen[id] {
				return errors.New("请勿重复选择账号")
			}
			seen[id] = true
			if r.Store.Get(req.Context(), "accounts", id, &a) != nil || a.Backend != "onedrive" || a.DriveType != "personal" || a.Region != "global" || !a.Verified {
				return errors.New("仅可导出已验证的 OneDrive 账号")
			}
			c, err := r.loadCredential(id)
			if err != nil {
				return err
			}
			entry := transferEntry{portableAccount(a), c}
			if err := validTransferEntry(entry); err != nil {
				return err
			}
			entry.Credential.GrantID = ""
			p.Entries = append(p.Entries, entry)
		}
		return nil
	}()
	r.credentialsMu.Unlock()
	r.accountMu.Unlock()
	if err != nil {
		fail(w, 400, "account_export_failed", err.Error())
		return
	}
	kit, err := protectAccounts(p, v.Password)
	if err != nil {
		fail(w, 400, "account_export_failed", err.Error())
		return
	}
	r.resetAttempts(req, "transfer")
	_ = r.Store.Audit(req.Context(), "accounts.export", "accounts")
	w.Header().Set("Content-Disposition", `attachment; filename="vaultpilot-accounts.json"`)
	writeJSON(w, 200, kit)
}
func (r *Runtime) transferExisting(req *http.Request) (map[string]bool, error) {
	all, err := r.Store.All(req.Context(), "accounts")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, raw := range all {
		var a Account
		if json.Unmarshal(raw, &a) == nil && a.Backend == "onedrive" && a.DriveID != "" {
			seen[a.DriveID] = true
		}
	}
	return seen, nil
}
func (r *Runtime) previewAccounts(w http.ResponseWriter, req *http.Request) {
	var v transferRequest
	if !r.decodeTransfer(w, req, &v, 2*maxAccountTransfer) {
		return
	}
	r.accountTransferMu.Lock()
	defer r.accountTransferMu.Unlock()
	p, err := openAccounts(v.Package, v.Password)
	if err != nil {
		fail(w, 400, "account_package_invalid", err.Error())
		return
	}
	r.resetAttempts(req, "transfer")
	seen, err := r.transferExisting(req)
	if err != nil {
		fail(w, 500, "database_error", "无法读取已有账号")
		return
	}
	items := []map[string]any{}
	for _, entry := range p.Entries {
		items = append(items, map[string]any{"account": portableAccount(entry.Account), "duplicate": seen[entry.Account.DriveID]})
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": len(items)})
}
func (r *Runtime) importAccounts(w http.ResponseWriter, req *http.Request) {
	var v transferRequest
	if !r.decodeTransfer(w, req, &v, 2*maxAccountTransfer) {
		return
	}
	r.accountTransferMu.Lock()
	defer r.accountTransferMu.Unlock()
	p, err := openAccounts(v.Package, v.Password)
	if err != nil {
		fail(w, 400, "account_package_invalid", err.Error())
		return
	}
	r.resetAttempts(req, "transfer")
	selected := map[string]bool{}
	if len(v.DriveIDs) < 1 || len(v.DriveIDs) > 50 {
		fail(w, 400, "account_selection_required", "请选择需要导入的账号")
		return
	}
	for _, id := range v.DriveIDs {
		selected[id] = true
	}
	r.accountMu.Lock()
	defer r.accountMu.Unlock()
	r.credentialsMu.Lock()
	defer r.credentialsMu.Unlock()
	seen, err := r.transferExisting(req)
	if err != nil {
		fail(w, 500, "database_error", "无法读取已有账号")
		return
	}
	items := []Account{}
	skipped := []Account{}
	sealed := map[string]string{}
	for _, entry := range p.Entries {
		if !selected[entry.Account.DriveID] {
			continue
		}
		delete(selected, entry.Account.DriveID)
		a := portableAccount(entry.Account)
		if seen[a.DriveID] {
			skipped = append(skipped, a)
			continue
		}
		seen[a.DriveID] = true
		a.ID = newID()
		a.Remote = "vp_" + a.ID
		a.Version = 1
		a.Status = "awaiting_drive_selection"
		a.Verified = false
		c := entry.Credential
		c.GrantID = newID()
		box, e := r.sealCredential(a.ID, c)
		if e != nil {
			fail(w, 500, "crypto_failed", "账号凭据加密失败")
			return
		}
		sealed[a.ID] = box
		items = append(items, a)
	}
	if len(selected) != 0 {
		fail(w, 400, "account_selection_invalid", "选择的 Drive 不属于此账号包")
		return
	}
	tx, err := r.Store.DB.BeginTx(req.Context(), nil)
	if err != nil {
		fail(w, 500, "database_error", "无法导入账号")
		return
	}
	defer tx.Rollback()
	for _, a := range items {
		raw, _ := json.Marshal(a)
		if _, err = tx.ExecContext(req.Context(), "INSERT INTO accounts(id,version,data,updated_at) VALUES(?,?,?,?)", a.ID, a.Version, string(raw), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			break
		}
		_, err = tx.ExecContext(req.Context(), "INSERT INTO metadata(key,value) VALUES(?,?)", "credential:"+a.ID, sealed[a.ID])
		if err != nil {
			break
		}
	}
	if err != nil {
		fail(w, 500, "database_error", "账号导入失败，未保存任何账号")
		return
	}
	if err = tx.Commit(); err != nil {
		fail(w, 500, "database_error", "账号导入提交失败")
		return
	}
	_ = r.Store.Audit(req.Context(), "accounts.import", "accounts")
	writeJSON(w, 201, map[string]any{"items": items, "skipped": skipped, "total": len(items)})
}
