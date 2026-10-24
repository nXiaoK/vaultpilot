package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"vaultpilot/internal/process"
)

// These settings are encrypted with the token. Rclone-mode credentials keep
// Rclone's defaults unless the imported remote explicitly used its own app.
type accountCredential struct {
	Provider     string     `json:"provider,omitempty"`
	Token        OAuthToken `json:"token"`
	Mode         string     `json:"mode"`
	GrantID      string     `json:"grant_id,omitempty"`
	ClientID     string     `json:"client_id,omitempty"`
	ClientSecret string     `json:"client_secret,omitempty"`
	Tenant       string     `json:"tenant,omitempty"`
	Scopes       string     `json:"scopes,omitempty"`
}

func credentialProvider(c accountCredential) string {
	if c.Provider == "" {
		return backendOneDrive
	}
	return c.Provider
}

func credentialTokenEndpoint(tenant string) string {
	if tenant == "" {
		tenant = "common"
	}
	return "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
}
func (r *Runtime) customCredential(t OAuthToken) accountCredential {
	return accountCredential{Token: t, Mode: "custom", ClientID: r.Config.OAuthClientID, ClientSecret: r.Config.OAuthClientSecret, Tenant: r.Config.OAuthTenant, Scopes: oauthScope}
}
func (r *Runtime) sealCredential(id string, c accountCredential) (string, error) {
	plain, e := json.Marshal(c)
	if e != nil {
		return "", e
	}
	defer func() {
		for i := range plain {
			plain[i] = 0
		}
	}()
	box, e := encrypt(r.nodeKey, plain, []byte("vaultpilot-oauth-v1:"+id))
	if e != nil {
		return "", e
	}
	return base64.StdEncoding.EncodeToString(box), nil
}
func (r *Runtime) saveCredential(id string, c accountCredential) error {
	raw, e := r.sealCredential(id, c)
	if e != nil {
		return e
	}
	return r.setMeta("credential:"+id, raw)
}
func (r *Runtime) loadCredential(id string) (accountCredential, error) {
	var c accountCredential
	raw, e := r.meta("credential:" + id)
	if e != nil || raw == "" {
		return c, errors.New("账号尚未授权")
	}
	box, e := base64.StdEncoding.DecodeString(raw)
	if e != nil {
		return c, errors.New("账号凭据格式损坏")
	}
	plain, e := decrypt(r.nodeKey, box, []byte("vaultpilot-oauth-v1:"+id))
	if e != nil {
		return c, errors.New("账号凭据无法解密")
	}
	defer func() {
		for i := range plain {
			plain[i] = 0
		}
	}()
	if json.Unmarshal(plain, &c) != nil {
		return c, errors.New("账号凭据格式损坏")
	}
	// Backward-compatible upgrade of the original token-only format.
	if c.Mode == "" {
		if c.Provider != "" {
			return c, errors.New("账号凭据格式损坏")
		}
		var t OAuthToken
		if json.Unmarshal(plain, &t) != nil {
			return c, errors.New("账号凭据格式损坏")
		}
		c = r.customCredential(t)
	}
	if c.Token.AccessToken == "" || !cloudBackend(credentialProvider(c)) || (c.Mode != "custom" && c.Mode != "rclone") || credentialProvider(c) == backendGoogleDrive && c.Mode != "rclone" {
		return c, errors.New("账号凭据格式损坏")
	}
	return c, nil
}
func (r *Runtime) loadToken(id string) (OAuthToken, error) {
	c, e := r.loadCredential(id)
	return c.Token, e
}
func (r *Runtime) saveToken(id string, t OAuthToken) error {
	c, e := r.loadCredential(id)
	if e != nil {
		c = r.customCredential(t)
	}
	c.Token = t
	return r.saveCredential(id, c)
}

// credentialConfig constructs an allowlisted remote: no arbitrary endpoints,
// executable hooks, proxy settings, or paths from an imported config survive.
func (r *Runtime) credentialConfig(ctx context.Context, name string, c accountCredential, driveID, driveType string) (string, error) {
	token, e := json.Marshal(c.Token)
	if e != nil {
		return "", e
	}
	provider := credentialProvider(c)
	if !cloudBackend(provider) || provider == backendGoogleDrive && c.Mode != "rclone" {
		return "", errors.New("账号凭据类型无效")
	}
	var b strings.Builder
	if provider == backendGoogleDrive {
		b.WriteString("[" + name + "]\ntype = drive\nscope = drive\ntoken = " + string(token) + "\n")
	} else {
		b.WriteString("[" + name + "]\ntype = onedrive\nregion = global\ntoken = " + string(token) + "\n")
	}
	if c.GrantID != "" {
		b.WriteString("vp_credential_id = " + c.GrantID + "\n")
	}
	if provider == backendOneDrive {
		if c.Scopes != "" {
			b.WriteString("access_scopes = " + c.Scopes + "\n")
		}
		// Rclone's noninteractive continuation uses its base OAuth config; supply
		// the standard common endpoint explicitly as well as imported overrides.
		b.WriteString("token_url = " + credentialTokenEndpoint(c.Tenant) + "\n")
	}
	if c.ClientID != "" {
		b.WriteString("client_id = " + c.ClientID + "\n")
	}
	if c.ClientSecret != "" {
		secret := c.ClientSecret
		if c.Mode == "custom" {
			cmd := exec.CommandContext(ctx, r.Config.RcloneBin, "obscure", "-")
			cmd.Env = rcloneEnvironment()
			cmd.Stdin = strings.NewReader(secret + "\n")
			output, err := cmd.Output()
			if err != nil {
				return "", errors.New("无法编码 Rclone 客户端密钥")
			}
			secret = strings.TrimSpace(string(output))
		}
		if strings.ContainsAny(secret, "\r\n\x00") || secret == "" {
			return "", errors.New("客户端密钥格式无效")
		}
		b.WriteString("client_secret = " + secret + "\n")
	}
	if provider == backendGoogleDrive {
		if driveID != "" {
			b.WriteString("root_folder_id = " + driveID + "\n")
		}
		b.WriteString("stop_on_upload_limit = true\nuse_trash = false\n")
		if r.Config.GoogleDriveTPSLimit > 0 {
			// This backend-local pacer also applies to Drive in a mixed-provider
			// copy, where a process-wide TPS limit would affect OneDrive too.
			interval := time.Duration(float64(time.Second) / r.Config.GoogleDriveTPSLimit)
			b.WriteString("pacer_min_sleep = " + interval.String() + "\npacer_burst = " + strconv.Itoa(r.googleDriveTPSBurst()) + "\n")
		}
	} else if driveID != "" {
		b.WriteString("drive_id = " + driveID + "\ndrive_type = " + driveType + "\n")
	}
	return b.String() + "\n", nil
}
func (r *Runtime) credentialFile(prefix, content string) (string, error) {
	f, e := os.CreateTemp(r.Config.CacheDir, prefix)
	if e != nil {
		return "", e
	}
	path := f.Name()
	e = f.Chmod(0600)
	if e == nil {
		_, e = f.WriteString(content)
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		os.Remove(path)
		return "", e
	}
	return path, nil
}

// Ask Rclone to enumerate drives via its noninteractive configuration API.
// This refreshes with the exact built-in/imported app without duplicating the
// shared application secret in VaultPilot. Output is private, never logged.
func (r *Runtime) refreshRcloneToken(ctx context.Context, id string, c accountCredential) (OAuthToken, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	refreshCredential := c
	if credentialProvider(c) == backendGoogleDrive {
		refreshCredential.Token.Expiry = time.Now().Add(-time.Hour)
	}
	content, e := r.credentialConfig(ctx, "vp_refresh", refreshCredential, "", "")
	if e != nil {
		return c.Token, e
	}
	path, e := r.credentialFile("oauth-refresh-*.conf", content)
	if e != nil {
		return c.Token, errors.New("无法创建凭据刷新配置")
	}
	defer os.Remove(path)
	args := []string{"config", "update", "vp_refresh", "--non-interactive", "--continue", "--state", "onedrive", "--result", "", "--config", path, "--log-level", "ERROR"}
	if credentialProvider(c) == backendGoogleDrive {
		args = []string{"about", "vp_refresh:", "--json", "--config", path, "--log-level", "ERROR"}
	}
	args = r.RcloneArgsFor([]Account{{Backend: credentialProvider(c)}}, args...)
	cmd := exec.CommandContext(ctx, r.Config.RcloneBin, args...)
	cmd.Env = rcloneEnvironment()
	output, e := cmd.Output()
	if e != nil {
		if ctx.Err() != nil {
			return c.Token, ctx.Err()
		}
		if exit, ok := e.(*exec.ExitError); ok {
			if credentialProvider(c) == backendGoogleDrive {
				output := strings.ToLower(string(exit.Stderr))
				for _, reason := range []string{"userRateLimitExceeded", "rateLimitExceeded", "dailyLimitExceeded", "storageQuotaExceeded"} {
					if strings.Contains(output, strings.ToLower(reason)) {
						return c.Token, cloudError{Status: 403, Provider: backendGoogleDrive, Reason: reason}
					}
				}
				if strings.Contains(output, "upload limit") {
					return c.Token, cloudError{Status: 403, Provider: backendGoogleDrive, Reason: "userRateLimitExceeded"}
				}
			}
			r.ObserveRcloneFailure(id, exit.Stderr, e)
			if credentialProvider(c) == backendGoogleDrive && strings.Contains(strings.ToLower(string(exit.Stderr)), "invalid_grant") {
				return c.Token, authorizationIssue{"authorization_required", "Google Drive 授权已失效，请重新授权"}
			}
		}
		return c.Token, authorizationIssue{"refresh_failed", "Rclone 刷新授权失败，请检查网络或重新授权"}
	}
	var result struct{ Error string }
	if credentialProvider(c) == backendOneDrive && (json.Unmarshal(output, &result) != nil || result.Error != "") {
		r.ObserveRcloneFailure(id, []byte(result.Error), errors.New("refresh failed"))
		return c.Token, authorizationIssue{"refresh_failed", "Rclone 无法刷新授权或读取 Drive，请重新授权"}
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return c.Token, errors.New("无法读取刷新凭据")
	}
	entries, e := parseRcloneImport(string(raw), credentialProvider(c))
	if e != nil || len(entries) != 1 {
		return c.Token, errors.New("刷新凭据格式无效")
	}
	fresh := entries[0].Credential.Token
	if fresh.AccessToken == "" || !fresh.Expiry.After(time.Now()) {
		return c.Token, authorizationIssue{"authorization_required", "刷新凭据已过期，请重新授权"}
	}
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = c.Token.RefreshToken
	}
	c.Token = fresh
	if e = r.saveCredential(id, c); e != nil {
		return fresh, errors.New("刷新凭据持久化失败")
	}
	return fresh, nil
}
func rcloneEnvironment() []string {
	return process.BaseEnvironment()
}
