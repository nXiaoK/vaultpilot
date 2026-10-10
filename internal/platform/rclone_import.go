package platform

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxRcloneImport = 1 << 20

var tenantPattern = regexp.MustCompile(`^(common|consumers|organizations|[a-zA-Z0-9][a-zA-Z0-9.-]{0,252})$`)

type rcloneImportEntry struct {
	Name         string            `json:"name"`
	DriveID      string            `json:"drive_id,omitempty"`
	DriveType    string            `json:"drive_type,omitempty"`
	CustomClient bool              `json:"custom_client"`
	Credential   accountCredential `json:"-"`
}

func parseRcloneImport(input string) ([]rcloneImportEntry, error) {
	bad := errors.New("无法解析配置：请导入未加密的 rclone.conf、config dump JSON 或 authorize 结果")
	if len(input) > maxRcloneImport {
		return nil, errors.New("配置文件不能超过 1 MiB")
	}
	input = strings.TrimSpace(strings.TrimPrefix(input, "\ufeff"))
	if strings.HasPrefix(input, "RCLONE_ENCRYPT_V") {
		return nil, errors.New("请先在本机解密 Rclone 配置，再导入；此入口不接收配置密码")
	}
	if strings.Contains(input, "--->") && strings.Contains(input, "<---End paste") {
		_, input, _ = strings.Cut(input, "--->")
		input, _, _ = strings.Cut(input, "<---End paste")
		input = strings.TrimSpace(input)
	}
	if input != "" && input[0] != '{' && input[0] != '[' {
		decoded, e := base64.RawURLEncoding.DecodeString(input)
		if e != nil {
			decoded, e = base64.URLEncoding.DecodeString(input)
		}
		if e != nil || len(decoded) > maxRcloneImport {
			return nil, bad
		}
		input = string(decoded)
	}
	configs := map[string]map[string]string{}
	if strings.HasPrefix(input, "{") {
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(input), &obj) != nil {
			return nil, bad
		}
		if _, ok := obj["access_token"]; ok {
			configs["授权结果"] = map[string]string{"type": "onedrive", "token": input}
		} else if _, ok := obj["token"]; ok {
			var v map[string]string
			if json.Unmarshal([]byte(input), &v) != nil {
				return nil, bad
			}
			if v["type"] == "" {
				v["type"] = "onedrive"
			}
			configs["授权结果"] = v
		} else {
			for name, raw := range obj {
				var v map[string]string
				if json.Unmarshal(raw, &v) != nil {
					return nil, bad
				}
				configs[name] = v
			}
		}
	} else {
		scan := bufio.NewScanner(strings.NewReader(input))
		scan.Buffer(make([]byte, 4096), maxRcloneImport)
		section := ""
		for scan.Scan() {
			line := strings.TrimSpace(scan.Text())
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				section = strings.TrimSpace(line[1 : len(line)-1])
				if section == "" || configs[section] != nil {
					return nil, errors.New("配置包含空名称或重复 remote")
				}
				configs[section] = map[string]string{}
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			k = strings.TrimSpace(k)
			if !ok || section == "" || k == "" {
				return nil, bad
			}
			if _, exists := configs[section][k]; exists {
				return nil, errors.New("配置含有重复选项")
			}
			configs[section][k] = strings.TrimSpace(v)
		}
		if scan.Err() != nil {
			return nil, bad
		}
	}
	entries := []rcloneImportEntry{}
	for name, config := range configs {
		if config["type"] != "onedrive" {
			continue
		}
		entry, e := validateRcloneEntry(name, config)
		if e != nil {
			return nil, e
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil, errors.New("配置中没有可导入的 OneDrive 授权")
	}
	if len(entries) > 100 {
		return nil, errors.New("每次最多导入包含 100 个 OneDrive remote 的配置")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}
func validateRcloneEntry(name string, v map[string]string) (rcloneImportEntry, error) {
	c := accountCredential{Mode: "rclone", ClientID: v["client_id"], ClientSecret: v["client_secret"], Scopes: v["access_scopes"], Tenant: v["tenant"]}
	bad := errors.New("OneDrive 配置无效或不受支持：仅支持全球区 Personal、标准微软端点和委托授权")
	for k, value := range v {
		if len(value) > 65536 || strings.ContainsAny(k+value, "\r\n\x00") {
			return rcloneImportEntry{}, bad
		}
	}
	if len(name) > 200 || strings.ContainsAny(name, "\r\n\x00") {
		return rcloneImportEntry{}, bad
	}
	if v["region"] != "" && v["region"] != "global" || v["drive_type"] != "" && v["drive_type"] != "personal" || v["client_credentials"] == "true" {
		return rcloneImportEntry{}, bad
	}
	if v["auth_url"] != "" && v["auth_url"] != "https://login.microsoftonline.com/common/oauth2/v2.0/authorize" {
		return rcloneImportEntry{}, bad
	}
	if c.Tenant != "" && !tenantPattern.MatchString(c.Tenant) {
		return rcloneImportEntry{}, bad
	}
	if v["token_url"] != "" {
		u, e := url.Parse(v["token_url"])
		if e != nil || u.Scheme != "https" || u.Host != "login.microsoftonline.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return rcloneImportEntry{}, bad
		}
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(parts) != 4 || parts[1] != "oauth2" || parts[2] != "v2.0" || parts[3] != "token" || !tenantPattern.MatchString(parts[0]) {
			return rcloneImportEntry{}, bad
		}
		c.Tenant = parts[0]
	}
	for _, scope := range strings.Fields(c.Scopes) {
		switch scope {
		case "offline_access", "Files.Read", "Files.ReadWrite", "Files.Read.All", "Files.ReadWrite.All", "Sites.Read.All", "User.Read":
		default:
			return rcloneImportEntry{}, errors.New("配置的 access_scopes 不受支持")
		}
	}
	if c.ClientSecret != "" && (c.ClientID == "" || len(c.ClientSecret) < 22) {
		return rcloneImportEntry{}, errors.New("自定义应用密钥须使用 rclone.conf 中的 obscure 格式并提供 client_id")
	}
	if c.ClientSecret != "" {
		secret, e := base64.RawURLEncoding.DecodeString(c.ClientSecret)
		if e != nil || len(secret) <= 16 {
			return rcloneImportEntry{}, errors.New("客户端密钥不是有效的 Rclone obscure 编码")
		}
	}
	if json.Unmarshal([]byte(v["token"]), &c.Token) != nil || c.Token.AccessToken == "" || c.Token.RefreshToken == "" || c.Token.Expiry.IsZero() || !strings.EqualFold(c.Token.TokenType, "Bearer") {
		return rcloneImportEntry{}, errors.New("授权结果缺少有效 token、refresh_token、Bearer 类型或 expiry；请重新使用 Rclone 授权")
	}
	if strings.ContainsAny(c.Token.AccessToken+c.Token.RefreshToken, "\r\n\x00") {
		return rcloneImportEntry{}, bad
	}
	return rcloneImportEntry{Name: name, DriveID: v["drive_id"], DriveType: v["drive_type"], CustomClient: c.ClientID != "", Credential: c}, nil
}

type rcloneImportRequest struct {
	Config  string `json:"config"`
	Remote  string `json:"remote"`
	Version int    `json:"version"`
}

func (r *Runtime) previewRcloneImport(w http.ResponseWriter, req *http.Request) {
	var v rcloneImportRequest
	if !decode(w, req, &v) {
		return
	}
	entries, e := parseRcloneImport(v.Config)
	if e != nil {
		fail(w, 400, "invalid_rclone_config", e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": entries, "total": len(entries)})
}
func (r *Runtime) importRcloneAccount(w http.ResponseWriter, req *http.Request) {
	var v rcloneImportRequest
	if !decode(w, req, &v) {
		return
	}
	entries, e := parseRcloneImport(v.Config)
	if e != nil {
		fail(w, 400, "invalid_rclone_config", e.Error())
		return
	}
	var selected *rcloneImportEntry
	for i := range entries {
		if entries[i].Name == v.Remote {
			selected = &entries[i]
			break
		}
	}
	if selected == nil {
		fail(w, 400, "remote_selection_required", "请显式选择本次导入的 remote")
		return
	}
	r.rcloneAuthMu.Lock()
	defer r.rcloneAuthMu.Unlock()
	for _, f := range r.rcloneAuth {
		if f.AccountID == req.PathValue("id") && f.active() {
			fail(w, 409, "authorization_in_progress", "请先取消正在进行的 Rclone 授权")
			return
		}
	}
	a, e := r.installCredential(req.Context(), req.PathValue("id"), v.Version, selected.Credential, "account.rclone_imported")
	if e != nil {
		fail(w, 409, "credential_import_failed", e.Error())
		return
	}
	writeJSON(w, 200, a)
}

// Persist credential and account status in one transaction. A stale form or
// deleted account never overwrites the credential of a different revision.
func (r *Runtime) installCredential(ctx context.Context, id string, version int, c accountCredential, audit string) (Account, error) {
	r.accountMu.Lock()
	defer r.accountMu.Unlock()
	r.credentialsMu.Lock()
	defer r.credentialsMu.Unlock()
	var a Account
	if r.Store.Get(ctx, "accounts", id, &a) != nil {
		return a, errors.New("账号不存在")
	}
	if a.Backend != "onedrive" {
		return a, errors.New("此账号不是 OneDrive")
	}
	if a.Version != version {
		return a, errors.New("账号配置已变更，请重新加载后授权或导入")
	}
	c.GrantID = newID()
	sealed, e := r.sealCredential(id, c)
	if e != nil {
		return a, errors.New("凭据加密失败")
	}
	a.Status = "awaiting_drive_selection"
	a.Verified = false
	a.Version++
	data, e := json.Marshal(a)
	if e != nil {
		return a, e
	}
	tx, e := r.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return a, e
	}
	defer tx.Rollback()
	res, e := tx.ExecContext(ctx, "UPDATE accounts SET version=?,data=?,updated_at=? WHERE id=? AND version=?", a.Version, string(data), time.Now().UTC().Format(time.RFC3339Nano), id, version)
	if e != nil {
		return a, e
	}
	n, e := res.RowsAffected()
	if e != nil || n != 1 {
		return a, errors.New("账号配置已变更，请重新加载")
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", "credential:"+id, sealed); e != nil {
		return a, e
	}
	if e = tx.Commit(); e != nil {
		return a, e
	}
	_ = r.Store.Audit(ctx, audit, id)
	return a, nil
}
