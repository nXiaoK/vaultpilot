package platform

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// RcloneConfig returns a restricted ephemeral file, never a frontend-visible
// configuration. An empty result signals a refresh or persistence failure.
func (r *Runtime) RcloneConfig() string {
	r.rcloneMu.Lock()
	defer r.rcloneMu.Unlock()
	if e := r.syncRcloneCredentials(); e != nil {
		return ""
	}
	accounts, e := r.Store.All(context.Background(), "accounts")
	if e != nil {
		return ""
	}
	var b strings.Builder
	secret := ""
	if r.Config.OAuthClientSecret != "" {
		// Rclone's password fields use its own reversible obscuring format.
		// Read the secret from stdin, keeping it out of argv and logs.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, r.Config.RcloneBin, "obscure", "-")
		cmd.Stdin = strings.NewReader(r.Config.OAuthClientSecret + "\n")
		output, e := cmd.Output()
		if e != nil {
			return ""
		}
		secret = strings.TrimSpace(string(output))
		if secret == "" || strings.ContainsAny(secret, "\r\n") {
			return ""
		}
	}
	for _, raw := range accounts {
		var a Account
		if json.Unmarshal(raw, &a) != nil {
			return ""
		}
		if a.Backend != "onedrive" || !a.Verified {
			continue
		}
		t, e := r.accessToken(context.Background(), a.ID)
		if e != nil {
			return ""
		}
		token, e := json.Marshal(t)
		if e != nil {
			return ""
		}
		b.WriteString("[" + a.Remote + "]\ntype = onedrive\n")
		// Keep refresh permissions identical to the consented web grant.
		// Rclone's defaults include broader All/Sites scopes that this app
		// neither requests nor needs for OneDrive Personal.
		b.WriteString("access_scopes = " + oauthScope + "\n")
		b.WriteString("token_url = " + r.tokenEndpoint() + "\n")
		b.WriteString("token = " + string(token) + "\ndrive_id = " + a.DriveID + "\ndrive_type = " + a.DriveType + "\n")
		if r.Config.OAuthClientID != "" {
			b.WriteString("client_id = " + r.Config.OAuthClientID + "\n")
		}
		if r.Config.OAuthClientSecret != "" {
			b.WriteString("client_secret = " + secret + "\n")
		}
		b.WriteString("region = global\n\n")
	}
	f, e := os.CreateTemp(r.Config.CacheDir, "rclone-*.conf")
	if e != nil {
		return ""
	}
	path := f.Name()
	if e = f.Chmod(0600); e == nil {
		_, e = f.WriteString(b.String())
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		os.Remove(path)
		return ""
	}
	r.rclonePath = path
	r.rclonePaths = append(r.rclonePaths, path)
	return path
}
func (r *Runtime) SyncRcloneCredentials() error {
	r.rcloneMu.Lock()
	defer r.rcloneMu.Unlock()
	return r.syncRcloneCredentials()
}
func (r *Runtime) syncRcloneCredentials() error {
	for _, path := range r.rclonePaths {
		if e := r.syncRcloneFile(path); e != nil {
			return e
		}
	}
	return nil
}

// ReleaseRcloneConfig persists refreshes and removes a completed task's file.
func (r *Runtime) ReleaseRcloneConfig(path string) error {
	r.rcloneMu.Lock()
	defer r.rcloneMu.Unlock()
	for i, candidate := range r.rclonePaths {
		if candidate == path {
			if e := r.syncRcloneFile(path); e != nil {
				return e
			}
			if e := os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
			r.rclonePaths = append(r.rclonePaths[:i], r.rclonePaths[i+1:]...)
			return nil
		}
	}
	return nil
}

func (r *Runtime) syncRcloneFile(path string) error {
	f, e := os.Open(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	defer f.Close()
	tokens := map[string]OAuthToken{}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 1<<20)
	section := ""
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "token" {
			continue
		}
		var token OAuthToken
		if e = json.Unmarshal([]byte(strings.TrimSpace(value)), &token); e != nil || token.AccessToken == "" {
			return errors.New("rclone 刷新凭据格式无效")
		}
		tokens[section] = token
	}
	if e = scan.Err(); e != nil {
		return e
	}
	accounts, e := r.Store.All(context.Background(), "accounts")
	if e != nil {
		return e
	}
	r.credentialsMu.Lock()
	defer r.credentialsMu.Unlock()
	for _, raw := range accounts {
		var a Account
		if e = json.Unmarshal(raw, &a); e != nil {
			return e
		}
		t, ok := tokens[a.Remote]
		if !ok {
			continue
		}
		old, e := r.loadToken(a.ID)
		if e != nil {
			return e
		}
		if t.AccessToken != old.AccessToken && t.Expiry.After(old.Expiry) {
			if t.RefreshToken == "" {
				t.RefreshToken = old.RefreshToken
			}
			if e = r.saveToken(a.ID, t); e != nil {
				return e
			}
		}
	}
	return nil
}
