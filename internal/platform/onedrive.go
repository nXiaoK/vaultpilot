package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const graphBase = "https://graph.microsoft.com/v1.0"
const oauthScope = "offline_access Files.ReadWrite User.Read"

type OAuthToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	Expiry       time.Time `json:"expiry"`
}
type cloudError struct {
	Status     int
	RetryAfter string
}

func (e cloudError) Error() string {
	if e.Status == 429 {
		return "微软接口限流，请稍后重试"
	}
	if e.Status == 401 || e.Status == 403 {
		return "授权已失效或未授予所需权限，请重新授权"
	}
	return fmt.Sprintf("微软接口请求失败（HTTP %d）", e.Status)
}
func (r *Runtime) tokenExchange(ctx context.Context, values url.Values) (OAuthToken, error) {
	return r.tokenExchangeFor(ctx, r.customCredential(OAuthToken{}), values)
}
func (r *Runtime) tokenExchangeFor(ctx context.Context, cred accountCredential, values url.Values) (OAuthToken, error) {
	values.Set("client_id", cred.ClientID)
	if cred.ClientSecret != "" {
		values.Set("client_secret", cred.ClientSecret)
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, credentialTokenEndpoint(cred.Tenant), strings.NewReader(values.Encode()))
	if e != nil {
		return OAuthToken{}, e
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, e := r.client.Do(req)
	if e != nil {
		return OAuthToken{}, errors.New("无法连接微软授权服务")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return OAuthToken{}, cloudError{res.StatusCode, res.Header.Get("Retry-After")}
	}
	var v struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if e = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&v); e != nil || v.AccessToken == "" || v.ExpiresIn <= 0 {
		return OAuthToken{}, errors.New("微软授权响应无效")
	}
	return OAuthToken{v.AccessToken, v.RefreshToken, v.TokenType, time.Now().Add(time.Duration(v.ExpiresIn) * time.Second)}, nil
}
func (r *Runtime) accessToken(ctx context.Context, id string) (token OAuthToken, err error) {
	defer func() { r.ObserveAccountAccess(id, err) }()
	r.credentialsMu.Lock()
	defer r.credentialsMu.Unlock()
	cred, e := r.loadCredential(id)
	t := cred.Token
	if e != nil {
		return t, authorizationIssue{"authorization_required", e.Error()}
	}
	if t.Expiry.After(time.Now().Add(2 * time.Minute)) {
		return t, nil
	}
	if t.RefreshToken == "" {
		return t, authorizationIssue{"authorization_required", "凭据已到期，请重新授权"}
	}
	if cred.Mode == "rclone" {
		return r.refreshRcloneToken(ctx, id, cred)
	}
	refreshed, e := r.tokenExchangeFor(ctx, cred, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}, "scope": {oauthScope}})
	if e != nil {
		if ctx.Err() != nil {
			return t, ctx.Err()
		}
		var cloud cloudError
		if errors.As(e, &cloud) && (cloud.Status == 400 || cloud.Status == 401 || cloud.Status == 403) {
			return t, authorizationIssue{"authorization_required", e.Error()}
		}
		return t, authorizationIssue{"refresh_failed", e.Error()}
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = t.RefreshToken
	}
	if e = r.saveToken(id, refreshed); e != nil {
		return t, errors.New("刷新凭据持久化失败")
	}
	return refreshed, nil
}
func (r *Runtime) graph(ctx context.Context, id, method, path string, body []byte, contentType string) ([]byte, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New("无效 Graph 路径")
	}
	t, e := r.accessToken(ctx, id)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, method, graphBase+path, bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+t.AccessToken)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, e := r.client.Do(req)
	if e != nil {
		return nil, errors.New("无法连接 OneDrive")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		err := cloudError{res.StatusCode, res.Header.Get("Retry-After")}
		// Optional /me identity lookup may lack User.Read even when Drive access
		// works. Only storage access failures create authorization incidents.
		if strings.HasPrefix(path, "/drives/") || strings.HasPrefix(path, "/me/drives") {
			r.ObserveAccountAccess(id, err)
		}
		return nil, err
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if e != nil {
		return nil, errors.New("读取 OneDrive 响应失败")
	}
	return b, nil
}
func cloudFailure(w http.ResponseWriter, e error) {
	var issue authorizationIssue
	if errors.As(e, &issue) && issue.category == "authorization_required" {
		fail(w, 409, "authorization_required", e.Error())
		return
	}
	var c cloudError
	if errors.As(e, &c) {
		if c.RetryAfter != "" {
			w.Header().Set("Retry-After", c.RetryAfter)
		}
		if c.Status == 429 {
			fail(w, 429, "cloud_throttled", e.Error())
			return
		}
		if c.Status == 401 || c.Status == 403 {
			fail(w, 409, "authorization_required", e.Error())
			return
		}
	}
	fail(w, 502, "cloud_access_failed", e.Error())
}
func (r *Runtime) authorize(w http.ResponseWriter, req *http.Request) {
	if r.Config.OAuthClientID == "" || r.Config.PublicURL == "" {
		fail(w, 409, "oauth_not_configured", "请先注册微软应用并配置 VP_OAUTH_CLIENT_ID 与 VP_PUBLIC_URL；回调为公开地址 /api/oauth/callback")
		return
	}
	u, e := url.Parse(r.Config.PublicURL)
	if e != nil || u.Host == "" || u.Scheme != "https" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
		fail(w, 409, "https_required", "公网 OAuth 回调必须使用 HTTPS")
		return
	}
	var a Account
	if e = r.Store.Get(req.Context(), "accounts", req.PathValue("id"), &a); e != nil {
		fail(w, 404, "account_not_found", "账号不存在")
		return
	}
	if a.Backend != "onedrive" {
		fail(w, 400, "invalid_backend", "本地联调账号无需微软授权")
		return
	}
	_, sid, ok := r.session(req)
	if !ok {
		fail(w, 401, "unauthenticated", "请先登录")
		return
	}
	state := newID() + newID()
	verifier := base64.RawURLEncoding.EncodeToString(randomBytes(48))
	sum := sha256.Sum256([]byte(verifier))
	r.mu.Lock()
	for id, v := range r.oauth {
		if time.Now().After(v.Expires) {
			delete(r.oauth, id)
		}
	}
	r.oauth[state] = oauthState{AccountID: a.ID, Verifier: verifier, SessionID: sid, Expires: time.Now().Add(10 * time.Minute), Version: a.Version}
	r.mu.Unlock()
	values := url.Values{"client_id": {r.Config.OAuthClientID}, "response_type": {"code"}, "redirect_uri": {strings.TrimRight(r.Config.PublicURL, "/") + "/api/oauth/callback"}, "response_mode": {"query"}, "scope": {oauthScope}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "prompt": {"select_account"}}
	writeJSON(w, 200, map[string]string{"authorization_url": "https://login.microsoftonline.com/" + url.PathEscape(r.Config.OAuthTenant) + "/oauth2/v2.0/authorize?" + values.Encode()})
}
func (r *Runtime) oauthCallback(w http.ResponseWriter, req *http.Request) {
	redirect := func(key, value string) {
		http.Redirect(w, req, "/#accounts?"+key+"="+url.QueryEscape(value), http.StatusSeeOther)
	}
	state := req.URL.Query().Get("state")
	r.mu.Lock()
	flow, ok := r.oauth[state]
	delete(r.oauth, state)
	r.mu.Unlock()
	_, sid, loggedIn := r.session(req)
	if !ok || time.Now().After(flow.Expires) || !loggedIn || sid != flow.SessionID {
		redirect("oauth_error", "state_invalid")
		return
	}
	if req.URL.Query().Get("error") != "" || req.URL.Query().Get("code") == "" {
		r.AccountAuthorizationFailed(flow.AccountID, "authorization_failed")
		redirect("oauth_error", "authorization_denied")
		return
	}
	t, e := r.tokenExchange(req.Context(), url.Values{"grant_type": {"authorization_code"}, "code": {req.URL.Query().Get("code")}, "redirect_uri": {strings.TrimRight(r.Config.PublicURL, "/") + "/api/oauth/callback"}, "code_verifier": {flow.Verifier}, "scope": {oauthScope}})
	if e != nil {
		r.AccountAuthorizationFailed(flow.AccountID, "authorization_failed")
		redirect("oauth_error", "token_exchange_failed")
		return
	}
	a, e := r.installCredential(req.Context(), flow.AccountID, flow.Version, r.customCredential(t), "account.authorized")
	if e != nil {
		redirect("oauth_error", "credential_save_failed")
		return
	}
	redirect("authorized", a.ID)
}

type driveOwner struct {
	User struct {
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
	} `json:"user"`
}

func applyOwner(a *Account, owner driveOwner) {
	if owner.User.Email != "" {
		a.AccountEmail = owner.User.Email
	}
	if owner.User.DisplayName != "" {
		a.AccountDisplayName = owner.User.DisplayName
	}
}

type Drive struct {
	Owner     driveOwner `json:"owner"`
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	DriveType string     `json:"drive_type"`
	Supported bool       `json:"supported"`
	Quota     struct {
		Total     *int64 `json:"total"`
		Used      *int64 `json:"used"`
		Remaining *int64 `json:"remaining"`
		State     string `json:"state"`
	} `json:"quota"`
}

func (r *Runtime) listDrives(ctx context.Context, id string) ([]Drive, error) {
	b, e := r.graph(ctx, id, "GET", "/me/drives", nil, "")
	if e != nil {
		return nil, e
	}
	var res struct {
		Value []struct {
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			DriveType string          `json:"driveType"`
			Quota     json.RawMessage `json:"quota"`
			Owner     driveOwner      `json:"owner"`
		} `json:"value"`
		NextLink string `json:"@odata.nextLink"`
	}
	if e = json.Unmarshal(b, &res); e != nil {
		return nil, errors.New("Drive 列表响应格式无效")
	}
	if res.NextLink != "" {
		return nil, errors.New("Drive 列表需要分页，当前仅支持 OneDrive Personal；请使用个人账号")
	}
	drives := make([]Drive, 0, len(res.Value))
	seen := map[string]int{}
	for _, d := range res.Value {
		if d.ID == "" {
			continue
		}
		v := Drive{ID: d.ID, Name: d.Name, DriveType: d.DriveType, Supported: d.DriveType == "personal", Owner: d.Owner}
		_ = json.Unmarshal(d.Quota, &v.Quota)
		if index, exists := seen[d.ID]; exists {
			if strings.EqualFold(v.Name, "OneDrive") {
				drives[index] = v
			}
			continue
		}
		seen[d.ID] = len(drives)
		drives = append(drives, v)
	}
	return drives, nil
}

// Selection metadata and optional identity are cached; access and quota checks
// always contact Graph. Identity remains transient until verification succeeds.
const driveListTTL = time.Minute
const profileTimeout = 3 * time.Second

type cachedDrives struct {
	Version int
	Expires time.Time
	Items   []Drive
	Profile *accountProfile
}

type accountProfile struct {
	Email, DisplayName string
	Expires            time.Time
}

// Profile is optional: older authorizations may lack User.Read.
func (r *Runtime) readAccountProfile(ctx context.Context, id string) *accountProfile {
	b, e := r.graph(ctx, id, "GET", "/me?$select=displayName,mail,userPrincipalName", nil, "")
	var profile struct{ DisplayName, Mail, UserPrincipalName string }
	if e != nil || json.Unmarshal(b, &profile) != nil {
		return nil
	}
	email := profile.Mail
	if email == "" && strings.Contains(profile.UserPrincipalName, "@") && !strings.Contains(profile.UserPrincipalName, "#EXT#") {
		email = profile.UserPrincipalName
	}
	if email == "" && profile.DisplayName == "" {
		return nil
	}
	return &accountProfile{Email: email, DisplayName: profile.DisplayName, Expires: time.Now().Add(driveListTTL)}
}

// Caller holds accountMu. Cache contains no authorization credentials.
func (r *Runtime) rememberDrives(a Account, items []Drive, profile *accountProfile) {
	if r.recentDrives == nil {
		r.recentDrives = make(map[string]cachedDrives)
	}
	now := time.Now()
	for id, entry := range r.recentDrives {
		if !entry.Expires.After(now) {
			delete(r.recentDrives, id)
		}
	}
	if len(r.recentDrives) >= 128 {
		for id := range r.recentDrives {
			delete(r.recentDrives, id)
			break
		}
	}
	r.recentDrives[a.ID] = cachedDrives{Version: a.Version, Expires: now.Add(driveListTTL), Items: items, Profile: profile}
}

func (r *Runtime) verificationDrives(ctx context.Context, a Account) ([]Drive, error) {
	if entry, ok := r.recentDrives[a.ID]; ok && entry.Version == a.Version && entry.Expires.After(time.Now()) {
		return entry.Items, nil
	}
	delete(r.recentDrives, a.ID)
	return r.listDrives(ctx, a.ID)
}

func (r *Runtime) drives(w http.ResponseWriter, req *http.Request) {
	var a Account
	r.accountMu.Lock()
	if r.Store.Get(req.Context(), "accounts", req.PathValue("id"), &a) != nil {
		r.accountMu.Unlock()
		fail(w, 404, "account_not_found", "账号不存在")
		return
	}
	var profile *accountProfile
	if entry, ok := r.recentDrives[a.ID]; ok && entry.Version == a.Version && entry.Expires.After(time.Now()) && entry.Profile != nil && entry.Profile.Expires.After(time.Now()) {
		profile = entry.Profile
	}
	r.accountMu.Unlock()
	if a.Backend != "onedrive" {
		fail(w, 400, "invalid_backend", "本地联调账号没有 Drive 列表")
		return
	}
	// Refresh once before starting concurrent reads, so optional profile lookup
	// cannot exhaust its short deadline while waiting for token renewal.
	if _, e := r.accessToken(req.Context(), a.ID); e != nil {
		cloudFailure(w, e)
		return
	}
	profileCtx, cancel := context.WithTimeout(req.Context(), profileTimeout)
	defer cancel()
	profileResult := make(chan *accountProfile, 1)
	if a.AccountEmail == "" && profile == nil {
		go func() {
			profileResult <- r.readAccountProfile(profileCtx, a.ID)
		}()
	} else {
		profileResult <- profile
	}
	items, e := r.listDrives(req.Context(), a.ID)
	if e != nil {
		cloudFailure(w, e)
		return
	}
	select {
	case profile = <-profileResult:
	case <-profileCtx.Done():
	}
	// Remote reads do not hold the global account lock. Reject results if the
	// account was edited, deleted or reauthorized while those reads were running.
	r.accountMu.Lock()
	defer r.accountMu.Unlock()
	var current Account
	if r.Store.Get(req.Context(), "accounts", a.ID, &current) != nil || current.Version != a.Version {
		fail(w, 409, "version_conflict", "账号配置已变更，请重新读取")
		return
	}
	if profile != nil && a.AccountEmail == "" {
		a.AccountEmail, a.AccountDisplayName = profile.Email, profile.DisplayName
	}
	r.rememberDrives(a, items, profile)
	writeJSON(w, 200, map[string]any{"items": items, "total": len(items), "account": a})
}
func (r *Runtime) ensureCloudFolder(ctx context.Context, a Account, parent, name string) (string, error) {
	parentPath := "/drives/" + url.PathEscape(a.DriveID) + parent
	b, e := r.graph(ctx, a.ID, "GET", parentPath+":/"+url.PathEscape(name), nil, "")
	if e == nil {
		var v struct {
			ID     string          `json:"id"`
			Folder json.RawMessage `json:"folder"`
		}
		if json.Unmarshal(b, &v) != nil || v.ID == "" || len(v.Folder) == 0 {
			return "", errors.New("项目目录被非目录项目占用")
		}
		return v.ID, nil
	}
	var c cloudError
	if !errors.As(e, &c) || c.Status != 404 {
		return "", e
	}
	body, _ := json.Marshal(map[string]any{"name": name, "folder": map[string]any{}, "@microsoft.graph.conflictBehavior": "fail"})
	childrenPath := parentPath + "/children"
	if strings.Contains(parent, ":") {
		childrenPath = parentPath + ":/children"
	}
	b, e = r.graph(ctx, a.ID, "POST", childrenPath, body, "application/json")
	if e != nil {
		return "", e
	}
	var v struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &v) != nil || v.ID == "" {
		return "", errors.New("创建项目目录响应无效")
	}
	return v.ID, nil
}
func (r *Runtime) verifyCloud(ctx context.Context, a *Account) error {
	if _, e := r.graph(ctx, a.ID, "GET", "/drives/"+url.PathEscape(a.DriveID)+"/root", nil, ""); e != nil {
		return e
	}
	if r.RecoveryMode() {
		// Recovery nodes validate read access without creating or probing a
		// writer's vault directory (pending cloud nodes have no vault ID yet).
		if _, e := r.graph(ctx, a.ID, "GET", "/drives/"+url.PathEscape(a.DriveID)+"/root:/VaultPilot", nil, ""); e != nil {
			return e
		}
		if e := r.cloudQuota(ctx, a); e != nil {
			return e
		}
		a.Verified = true
		a.Status = "ready"
		return nil
	}
	root, e := r.ensureCloudFolder(ctx, *a, "/root", "VaultPilot")
	if e != nil {
		return e
	}
	vault, e := r.ensureCloudFolder(ctx, *a, "/items/"+url.PathEscape(root), r.VaultID())
	if e != nil {
		return e
	}
	verifyName := ".verification-" + newID() + ".bin"
	content := randomBytes(48)
	path := "/drives/" + url.PathEscape(a.DriveID) + "/items/" + url.PathEscape(vault) + ":/" + url.PathEscape(verifyName) + ":/content"
	response, e := r.graph(ctx, a.ID, "PUT", path, content, "application/octet-stream")
	if e != nil {
		return e
	}
	var file struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(response, &file) != nil || file.ID == "" {
		return errors.New("写入验证响应无效")
	}
	cleanup := func() {
		_, _ = r.graph(ctx, a.ID, "DELETE", "/drives/"+url.PathEscape(a.DriveID)+"/items/"+url.PathEscape(file.ID), nil, "")
	}
	defer cleanup()
	read, e := r.graph(ctx, a.ID, "GET", "/drives/"+url.PathEscape(a.DriveID)+"/items/"+url.PathEscape(file.ID)+"/content", nil, "")
	if e != nil || !bytes.Equal(read, content) {
		return errors.New("OneDrive 写入后读回验证失败")
	}
	if e = r.cloudQuota(ctx, a); e != nil {
		return e
	}
	a.Verified = true
	a.Status = "ready"
	return nil
}
func (r *Runtime) cloudQuota(ctx context.Context, a *Account) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	projectPath := "/root:/VaultPilot"
	if r.VaultID() != "" {
		projectPath += "/" + r.VaultID()
	}
	type projectResponse struct {
		body []byte
		err  error
	}
	projectResult := make(chan projectResponse, 1)
	accountID, drivePath := a.ID, "/drives/"+url.PathEscape(a.DriveID)
	// Drive quota and project occupancy are independent reads. Keep all Account
	// mutation in this goroutine after both responses have passed validation.
	go func() {
		b, e := r.graph(ctx, accountID, "GET", drivePath+projectPath, nil, "")
		projectResult <- projectResponse{b, e}
	}()
	b, e := r.graph(ctx, a.ID, "GET", "/drives/"+url.PathEscape(a.DriveID), nil, "")
	if e != nil {
		return e
	}
	var d struct {
		ID    string                                  `json:"id"`
		Owner driveOwner                              `json:"owner"`
		Quota struct{ Total, Used, Remaining *int64 } `json:"quota"`
	}
	if json.Unmarshal(b, &d) != nil || d.ID != a.DriveID || d.Quota.Total == nil || d.Quota.Used == nil || d.Quota.Remaining == nil {
		return errors.New("OneDrive 未返回完整配额，禁止把未知空间视为无限容量")
	}
	if *d.Quota.Total < 0 || *d.Quota.Used < 0 || *d.Quota.Remaining < 0 {
		return errors.New("OneDrive 配额响应无效")
	}
	var result projectResponse
	select {
	case result = <-projectResult:
	case <-ctx.Done():
		return ctx.Err()
	}
	projectUsed := int64(0)
	if result.err != nil {
		var c cloudError
		if !errors.As(result.err, &c) || c.Status != 404 {
			return result.err
		}
	} else {
		var p struct {
			Size *int64 `json:"size"`
		}
		if json.Unmarshal(result.body, &p) != nil || p.Size == nil || *p.Size < 0 {
			return errors.New("无法核对 OneDrive 项目占用")
		}
		projectUsed = *p.Size
	}
	applyOwner(a, d.Owner)
	a.TotalBytes = *d.Quota.Total
	a.UsedBytes = *d.Quota.Used
	a.FreeBytes = *d.Quota.Remaining
	a.ProjectUsedBytes = projectUsed
	a.QuotaAt = time.Now().UTC().Format(time.RFC3339)
	return nil
}
func (r *Runtime) verifyAccount(w http.ResponseWriter, req *http.Request) {
	r.accountMu.Lock()
	defer r.accountMu.Unlock()
	var a Account
	if r.Store.Get(req.Context(), "accounts", req.PathValue("id"), &a) != nil {
		fail(w, 404, "account_not_found", "账号不存在")
		return
	}
	var v struct {
		DriveID                 string `json:"drive_id"`
		Version                 int    `json:"version"`
		BudgetBytes             *int64 `json:"budget_bytes"`
		CloudReserveBytes       *int64 `json:"cloud_reserve_bytes"`
		MaintenanceReserveBytes *int64 `json:"maintenance_reserve_bytes"`
	}
	if !decode(w, req, &v) {
		return
	}
	if v.Version != a.Version {
		fail(w, 409, "version_conflict", "配置已变更，请重新加载")
		return
	}
	if v.BudgetBytes != nil {
		a.BudgetBytes = *v.BudgetBytes
	}
	if v.CloudReserveBytes != nil {
		a.CloudReserveBytes = *v.CloudReserveBytes
	}
	if v.MaintenanceReserveBytes != nil {
		a.MaintenanceReserveBytes = *v.MaintenanceReserveBytes
	}
	if e := validBudget(a); e != nil {
		fail(w, 400, "invalid_budget", e.Error())
		return
	}
	var e error
	if a.Backend == "local" {
		e = r.verifyLocal(req.Context(), &a)
	} else {
		if v.DriveID == "" {
			fail(w, 400, "drive_selection_required", "必须显式选择 Drive，不会默认选择列表第一项")
			return
		}
		list, err := r.verificationDrives(req.Context(), a)
		if err != nil {
			cloudFailure(w, err)
			return
		}
		var selected *Drive
		for i := range list {
			if list[i].ID == v.DriveID {
				selected = &list[i]
				break
			}
		}
		if selected == nil {
			fail(w, 400, "drive_not_authorized", "所选 Drive 不属于本次授权可见列表")
			return
		}
		if a.DriveID != "" && a.DriveID != selected.ID {
			fail(w, 409, "drive_identity_changed", "重新授权必须选择原 Drive；其他 Drive 请添加为新账号，以免历史仓库指向错误位置")
			return
		}
		if !selected.Supported {
			fail(w, 400, "drive_type_unsupported", "当前验证目标为 OneDrive Personal；企业版与 SharePoint 尚未完成兼容验证")
			return
		}
		a.DriveID = selected.ID
		a.DriveName = selected.Name
		applyOwner(&a, selected.Owner)
		a.DriveType = selected.DriveType
		a.Region = "global"
		a.TenantID = "consumers"
		a.QuotaDomain = "global:personal:" + a.DriveID
		all, err := r.Store.All(req.Context(), "accounts")
		if err != nil {
			fail(w, 500, "database_error", "无法检查重复 Drive")
			return
		}
		for _, raw := range all {
			var other Account
			_ = json.Unmarshal(raw, &other)
			if other.ID != a.ID && other.QuotaDomain == a.QuotaDomain {
				fail(w, 409, "duplicate_drive", "该 Drive 已登记，不能重复计算容量；请重新授权已有账号")
				return
			}
		}
		e = r.verifyCloud(req.Context(), &a)
	}
	if e != nil {
		cloudFailure(w, e)
		return
	}
	if a.Backend == "onedrive" && a.AccountEmail == "" {
		var profile *accountProfile
		if entry, ok := r.recentDrives[a.ID]; ok && entry.Version == a.Version && entry.Expires.After(time.Now()) && entry.Profile != nil && entry.Profile.Expires.After(time.Now()) {
			profile = entry.Profile
		}
		if profile == nil {
			ctx, cancel := context.WithTimeout(req.Context(), profileTimeout)
			profile = r.readAccountProfile(ctx, a.ID)
			cancel()
		}
		if profile != nil {
			a.AccountEmail, a.AccountDisplayName = profile.Email, profile.DisplayName
		}
	}
	ver, e := r.Store.Save(req.Context(), "accounts", a.ID, v.Version, a)
	a.Version = ver
	saved(w, 200, e, a)
	if e == nil {
		r.AccountAuthorizationHealthy(a.ID, true)
		_ = r.Store.Audit(req.Context(), "account.verified", a.ID)
	}
}
func (r *Runtime) refreshQuota(w http.ResponseWriter, req *http.Request) {
	var a Account
	if r.Store.Get(req.Context(), "accounts", req.PathValue("id"), &a) != nil {
		fail(w, 404, "account_not_found", "账号不存在")
		return
	}
	if !a.Verified {
		fail(w, 409, "account_unverified", "账号尚未通过访问验证")
		return
	}
	var e error
	if a.Backend == "local" {
		e = r.localQuota(&a)
	} else {
		e = r.cloudQuota(req.Context(), &a)
	}
	if e != nil {
		cloudFailure(w, e)
		return
	}
	v, e := r.Store.Save(req.Context(), "accounts", a.ID, a.Version, a)
	a.Version = v
	saved(w, 200, e, a)
}
