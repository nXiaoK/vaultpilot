package platform

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (r *Runtime) session(req *http.Request) (session, string, bool) {
	c, e := req.Cookie("vaultpilot_session")
	if e != nil {
		return session{}, "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[c.Value]
	if !ok || time.Now().After(s.Expires) {
		delete(r.sessions, c.Value)
		return session{}, "", false
	}
	return s, c.Value, true
}
func (r *Runtime) makeSession(w http.ResponseWriter, username string) session {
	id := newID() + newID()
	s := session{username, newID() + newID(), time.Now().Add(12 * time.Hour)}
	r.mu.Lock()
	for id, s := range r.sessions {
		if time.Now().After(s.Expires) {
			delete(r.sessions, id)
		}
	}
	r.sessions[id] = s
	r.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "vaultpilot_session", Value: id, Path: "/", HttpOnly: true, Secure: r.secureCookie(), SameSite: http.SameSiteLaxMode, MaxAge: 12 * 3600})
	return s
}
func (r *Runtime) allowedHost(host string) bool {
	host = strings.ToLower(host)
	name, _, e := net.SplitHostPort(host)
	if e != nil {
		name = host
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") && net.ParseIP(host[1:len(host)-1]) != nil {
			name = host[1 : len(host)-1]
		}
	}
	if name == "localhost" || name == "127.0.0.1" || name == "::1" {
		return true
	}
	if public, e := url.Parse(r.Config.PublicURL); e == nil && public.Host != "" && strings.EqualFold(public.Host, host) {
		return true
	}
	for _, allowed := range r.Config.AllowedHosts {
		if strings.EqualFold(strings.TrimSpace(allowed), host) {
			return true
		}
	}
	return false
}
func (r *Runtime) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !r.allowedHost(req.Host) {
			message := "请求主机名不在允许列表中，请通过 VP_PUBLIC_URL 或 VP_ALLOWED_HOSTS 配置访问地址"
			if strings.HasPrefix(req.URL.Path, "/api/") {
				fail(w, http.StatusMisdirectedRequest, "host_invalid", message)
			} else {
				http.Error(w, message, http.StatusMisdirectedRequest)
			}
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(req.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			public := req.Method == "GET" && (req.URL.Path == "/api/status" || req.URL.Path == "/api/oauth/callback") || req.Method == "POST" && (req.URL.Path == "/api/setup" || req.URL.Path == "/api/auth/login")
			if !public {
				s, _, ok := r.session(req)
				if !ok {
					fail(w, 401, "unauthenticated", "请先登录")
					return
				}
				if req.Method != "GET" && req.Method != "HEAD" && req.Method != "OPTIONS" {
					if subtle.ConstantTimeCompare([]byte(req.Header.Get("X-CSRF-Token")), []byte(s.CSRF)) != 1 {
						fail(w, 403, "csrf_invalid", "请求校验失败，请刷新页面后重试")
						return
					}
				}
			}
			if req.Method != "GET" && req.Method != "HEAD" {
				if origin := req.Header.Get("Origin"); origin != "" {
					u, e := url.Parse(origin)
					expected, e2 := url.Parse(r.Config.PublicURL)
					if e != nil || u.Host != req.Host && (e2 != nil || expected.Host == "" || u.Host != expected.Host) {
						fail(w, 403, "origin_invalid", "请求来源无效")
						return
					}
				}
			}
			next.ServeHTTP(w, req)
			return
		}
		if req.Method != "GET" && req.Method != "HEAD" {
			http.NotFound(w, req)
			return
		}
		clean := filepath.Clean(filepath.FromSlash(req.URL.Path))
		if strings.ContainsRune(clean, 0) {
			http.NotFound(w, req)
			return
		}
		path := filepath.Join("web/dist", strings.TrimPrefix(clean, string(filepath.Separator)))
		info, e := os.Stat(path)
		if e == nil && !info.IsDir() {
			http.ServeFile(w, req, path)
			return
		}
		if _, e = os.Stat("web/dist/index.html"); e == nil {
			http.ServeFile(w, req, "web/dist/index.html")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(503)
		_, _ = w.Write([]byte("VaultPilot 前端尚未构建。请运行 npm ci && npm run build（web 目录）。"))
	})
}
func (r *Runtime) status(w http.ResponseWriter, req *http.Request) {
	s, _, ok := r.session(req)
	v := map[string]any{"initialized": r.Initialized(), "authenticated": ok}
	if ok {
		r.mu.Lock()
		v["mode"] = r.mode
		r.mu.Unlock()
		v["unlocked"] = r.Keys.Unlocked()
		v["vault_id"] = r.VaultID()
		v["cloud_pending"] = r.CloudPending()
		v["oauth_configured"] = r.Config.OAuthClientID != "" && r.Config.PublicURL != ""
		v["allow_local"] = r.Config.AllowLocal
		v["engines"] = map[string]bool{"restic": engineAvailable(r.Config.ResticBin), "rclone": engineAvailable(r.Config.RcloneBin)}
		v["source_roots"] = r.Config.SourceRoots
		v["restore_roots"] = r.Config.RestoreRoots
		v["csrf_token"] = s.CSRF
		v["cache_max_bytes"] = r.Config.CacheMaxBytes
		v["log_max_rows"] = r.Config.LogMaxRows
		v["maintenance_window"] = r.Config.MaintenanceWindow
		v["maintenance_timezone"] = r.Config.MaintenanceTimezone
		var pending []string
		if raw, e := r.Store.Meta(req.Context(), "account_clear_pending"); e == nil && json.Unmarshal([]byte(raw), &pending) == nil {
			v["account_clear_pending"] = pending
		}
	}
	writeJSON(w, 200, v)
}
func (r *Runtime) trustedProxy(addr netip.Addr) bool {
	for _, prefix := range r.Config.TrustedProxies {
		if prefix.Contains(addr) || addr.Is4() && prefix.Addr().Is4In6() && prefix.Contains(netip.AddrFrom16(addr.As16())) {
			return true
		}
	}
	return false
}
func (r *Runtime) clientIP(req *http.Request) string {
	host, _, e := net.SplitHostPort(req.RemoteAddr)
	if e != nil {
		host = req.RemoteAddr
	}
	remote, e := netip.ParseAddr(host)
	if e != nil {
		return host
	}
	remote = remote.Unmap()
	if !r.trustedProxy(remote) {
		return remote.String()
	}
	forwarded := strings.Split(strings.Join(req.Header.Values("X-Forwarded-For"), ","), ",")
	current := remote
	for i := len(forwarded) - 1; i >= 0; i-- {
		if !r.trustedProxy(current) {
			return current.String()
		}
		addr, e := netip.ParseAddr(strings.TrimSpace(forwarded[i]))
		if e != nil {
			return remote.String()
		}
		current = addr.Unmap()
	}
	return current.String()
}
func (r *Runtime) resetAttempts(req *http.Request, action string) {
	key := action + "|" + r.clientIP(req)
	r.mu.Lock()
	delete(r.attempts, key)
	r.mu.Unlock()
}
func (r *Runtime) throttled(req *http.Request, action string) bool {
	key := action + "|" + r.clientIP(req)
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, a := range r.attempts {
		if now.Sub(a.Since) > 15*time.Minute {
			delete(r.attempts, k)
		}
	}
	a := r.attempts[key]
	if a.Since.IsZero() {
		a.Since = now
	}
	a.Count++
	r.attempts[key] = a
	return a.Count > 10
}
func (r *Runtime) setup(w http.ResponseWriter, req *http.Request) {
	r.setupMu.Lock()
	defer r.setupMu.Unlock()
	if r.Initialized() {
		fail(w, 409, "already_initialized", "实例已初始化")
		return
	}
	if r.throttled(req, "setup") {
		w.Header().Set("Retry-After", "900")
		fail(w, 429, "rate_limited", "请求过于频繁，请稍后重试")
		return
	}
	var input struct {
		Username         string          `json:"username"`
		Password         string          `json:"password"`
		RecoveryPassword string          `json:"recovery_password"`
		Mode             string          `json:"mode"`
		Kit              json.RawMessage `json:"recovery_kit"`
	}
	if !decode(w, req, &input) {
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	if input.Username == "" || len(input.Username) > 128 || len(input.Password) < 12 || len(input.Password) > 1024 || input.Mode != "cloud" && (len(input.RecoveryPassword) < 12 || len(input.RecoveryPassword) > 1024) {
		fail(w, 400, "weak_credentials", "管理员名称不能为空；登录密码与恢复密码分别至少 12 个字符")
		return
	}
	if input.RecoveryPassword != "" && input.Password == input.RecoveryPassword {
		fail(w, 400, "separate_credentials", "请使用与登录密码不同的恢复密码")
		return
	}
	var p RecoveryPayload
	var kit RecoveryKit
	var e error
	if input.Mode == "cloud" {
		// Credentials are local to this node. The original vault and master key
		// are installed only after authenticating a cloud bootstrap.
		p.Entries = []DiscoveryEntry{}
	} else if input.Mode == "recovery" {
		b := input.Kit
		if len(b) > 0 && b[0] == '"' {
			var s string
			if json.Unmarshal(b, &s) == nil {
				b = []byte(s)
			}
		}
		if json.Unmarshal(b, &kit) != nil {
			fail(w, 400, "invalid_recovery_kit", "请输入完整恢复包 JSON")
			return
		}
		p, e = openRecovery(kit, input.RecoveryPassword)
		if e != nil {
			fail(w, 400, "recovery_kit_invalid", e.Error())
			return
		}
	} else if input.Mode == "" || input.Mode == "create" {
		input.Mode = "create"
		p = RecoveryPayload{Version: 1, VaultID: newID(), MasterKey: base64.StdEncoding.EncodeToString(randomBytes(32)), Entries: []DiscoveryEntry{}}
		kit, e = protectRecovery(p, input.RecoveryPassword)
	} else {
		fail(w, 400, "invalid_mode", "请选择创建备份库、连接云端备份库或导入恢复包")
		return
	}
	if e != nil {
		fail(w, 500, "crypto_failed", "恢复包封装失败")
		return
	}
	hash := hashPassword(input.Password)
	hashJSON, _ := json.Marshal(hash)
	kitJSON, _ := json.Marshal(kit)
	entriesJSON, _ := json.Marshal(p.Entries)
	metadata := map[string]string{"admin_username": input.Username, "admin_password": string(hashJSON), "discovery_entries": string(entriesJSON), "mode": input.Mode, "vault_id": p.VaultID}
	if input.Mode != "cloud" {
		metadata["recovery_kit"] = string(kitJSON)
	}
	tx, e := r.Store.DB.BeginTx(req.Context(), nil)
	if e != nil {
		fail(w, 500, "database_error", "初始化失败")
		return
	}
	defer tx.Rollback()
	for key, value := range metadata {
		if _, e = tx.ExecContext(req.Context(), "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value); e != nil {
			fail(w, 500, "database_error", "初始化失败")
			return
		}
	}
	if e = tx.Commit(); e != nil {
		fail(w, 500, "database_error", "初始化提交失败")
		return
	}
	r.mu.Lock()
	r.vaultID = p.VaultID
	r.mode = input.Mode
	r.mu.Unlock()
	if input.Mode != "cloud" {
		key, _ := base64.StdEncoding.DecodeString(p.MasterKey)
		_ = r.Keys.set(key)
		for i := range key {
			key[i] = 0
		}
	}
	r.resetAttempts(req, "setup")
	s := r.makeSession(w, input.Username)
	writeJSON(w, 201, map[string]any{"vault_id": p.VaultID, "mode": input.Mode, "cloud_pending": input.Mode == "cloud", "csrf_token": s.CSRF, "unlocked": r.Keys.Unlocked()})
}
func (r *Runtime) login(w http.ResponseWriter, req *http.Request) {
	if r.throttled(req, "login") {
		w.Header().Set("Retry-After", "900")
		fail(w, 429, "rate_limited", "登录尝试过于频繁，请 15 分钟后重试")
		return
	}
	var v struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, req, &v) {
		return
	}
	if len(v.Password) > 1024 {
		fail(w, 400, "invalid_password", "密码长度超过限制")
		return
	}
	username, _ := r.meta("admin_username")
	b, _ := r.meta("admin_password")
	var h PasswordHash
	valid := json.Unmarshal([]byte(b), &h) == nil && verifyPassword(v.Password, h)
	if !valid || username != v.Username || !r.Initialized() {
		fail(w, 401, "credentials_invalid", "用户名或密码错误")
		return
	}
	r.resetAttempts(req, "login")
	s := r.makeSession(w, username)
	writeJSON(w, 200, map[string]any{"csrf_token": s.CSRF, "unlocked": r.Keys.Unlocked()})
}
func (r *Runtime) logout(w http.ResponseWriter, req *http.Request) {
	_, id, _ := r.session(req)
	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
	r.rcloneAuthMu.Lock()
	for _, flow := range r.rcloneAuth {
		if flow.SessionID == id && flow.active() {
			flow.Status = "cancelling"
			flow.cancel()
		}
	}
	r.rcloneAuthMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "vaultpilot_session", Value: "", Path: "/", HttpOnly: true, Secure: r.secureCookie(), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	w.WriteHeader(204)
}
func (r *Runtime) unlockHandler(w http.ResponseWriter, req *http.Request) {
	if r.throttled(req, "unlock") {
		fail(w, 429, "rate_limited", "解锁尝试过于频繁")
		return
	}
	var v struct {
		Password string `json:"password"`
	}
	if !decode(w, req, &v) {
		return
	}
	if e := r.unlock(v.Password); e != nil {
		fail(w, 400, "unlock_failed", e.Error())
		return
	}
	r.resetAttempts(req, "unlock")
	writeJSON(w, 200, map[string]bool{"unlocked": true})
}
func (r *Runtime) lockHandler(w http.ResponseWriter, req *http.Request) {
	r.Keys.Lock()
	writeJSON(w, 200, map[string]bool{"unlocked": false})
}
func (r *Runtime) exportKit(w http.ResponseWriter, req *http.Request) {
	var v struct {
		Password string `json:"password"`
	}
	if !decode(w, req, &v) {
		return
	}
	if len(v.Password) < 12 || len(v.Password) > 1024 {
		fail(w, 400, "weak_credentials", "恢复包保护密码至少 12 个字符")
		return
	}
	r.Keys.mu.RLock()
	if len(r.Keys.key) != 32 {
		r.Keys.mu.RUnlock()
		fail(w, 423, "vault_locked", "请先解锁备份库")
		return
	}
	master := base64.StdEncoding.EncodeToString(r.Keys.key)
	r.Keys.mu.RUnlock()
	entries := []DiscoveryEntry{}
	prior, e := r.meta("discovery_entries")
	if e != nil {
		fail(w, 500, "database_error", "无法读取发现入口")
		return
	}
	if prior != "" && json.Unmarshal([]byte(prior), &entries) != nil {
		fail(w, 500, "discovery_entries_invalid", "恢复发现入口已损坏")
		return
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.Backend+":"+entry.DriveID] = true
	}
	accounts, e := r.Store.All(req.Context(), "accounts")
	if e != nil {
		fail(w, 500, "database_error", "无法读取账号发现入口")
		return
	}
	for _, b := range accounts {
		var a Account
		if json.Unmarshal(b, &a) == nil && a.DriveID != "" && !seen[a.Backend+":"+a.DriveID] {
			entries = append(entries, DiscoveryEntry{Backend: a.Backend, DriveID: a.DriveID, Region: a.Region, TenantID: a.TenantID, LocalPath: a.LocalPath, Path: "VaultPilot/" + r.VaultID() + "/catalog/"})
			seen[a.Backend+":"+a.DriveID] = true
		}
	}
	kit, e := protectRecovery(RecoveryPayload{Version: 1, VaultID: r.VaultID(), MasterKey: master, Entries: entries}, v.Password)
	if e != nil {
		fail(w, 500, "crypto_failed", "导出恢复包失败")
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"vaultpilot-%s-recovery.json\"", r.VaultID()))
	_ = r.Store.Audit(req.Context(), "recovery-kit.export", r.VaultID())
	writeJSON(w, 200, kit)
}
