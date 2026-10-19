package platform

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type rcloneAuthFlow struct {
	ID, AccountID, Backend, SessionID, State, Status, Message, AuthorizationURL string
	Version                                                                     int
	Expires                                                                     time.Time
	Account                                                                     *Account
	cancel                                                                      context.CancelFunc
}

func (f *rcloneAuthFlow) active() bool {
	return f.Status == "starting" || f.Status == "waiting" || f.Status == "exchanging" || f.Status == "cancelling"
}
func (f *rcloneAuthFlow) view() map[string]any {
	return map[string]any{"id": f.ID, "account_id": f.AccountID, "status": f.Status, "message": f.Message, "authorization_url": f.AuthorizationURL, "expires_at": f.Expires, "account": f.Account}
}

var localAuthLink = regexp.MustCompile(`http://127\.0\.0\.1:53682/auth\?state=[A-Za-z0-9_-]+`)

type privateOutput struct{ buffer bytes.Buffer }

func (b *privateOutput) String() string { return b.buffer.String() }
func (b *privateOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > maxRcloneImport {
		return 0, errors.New("authorization output too large")
	}
	return b.buffer.Write(p)
}

func (r *Runtime) startRcloneAuth(w http.ResponseWriter, req *http.Request) {
	var v struct {
		Version int `json:"version"`
	}
	if !decode(w, req, &v) {
		return
	}
	var a Account
	if r.Store.Get(req.Context(), "accounts", req.PathValue("id"), &a) != nil {
		fail(w, 404, "account_not_found", "账号不存在")
		return
	}
	if !cloudBackend(a.Backend) || a.Version != v.Version {
		fail(w, 409, "account_changed", "请重新加载 "+backendLabel(a.Backend)+" 账号配置")
		return
	}
	if !engineAvailable(r.Config.RcloneBin) {
		fail(w, 409, "rclone_unavailable", "服务器未安装 Rclone，请先安装或导入已有配置")
		return
	}
	_, sid, _ := r.session(req)
	r.rcloneAuthMu.Lock()
	if r.closing {
		r.rcloneAuthMu.Unlock()
		fail(w, 503, "server_closing", "服务正在关闭")
		return
	}
	if r.rcloneAuth == nil {
		r.rcloneAuth = map[string]*rcloneAuthFlow{}
	}
	for id, f := range r.rcloneAuth {
		if f.active() {
			r.rcloneAuthMu.Unlock()
			fail(w, 409, "rclone_authorization_busy", "已有 Rclone 授权正在进行，请完成或取消后再试")
			return
		}
		if time.Now().After(f.Expires) {
			delete(r.rcloneAuth, id)
		}
	}
	if len(r.rcloneAuth) >= 16 {
		r.rcloneAuthMu.Unlock()
		fail(w, 429, "authorization_limit", "授权尝试过于频繁，请稍后重试")
		return
	}
	path, e := r.credentialFile("oauth-authorize-*.conf", "")
	if e != nil {
		r.rcloneAuthMu.Unlock()
		fail(w, 500, "authorization_start_failed", "无法创建授权临时配置")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	f := &rcloneAuthFlow{ID: newID(), AccountID: a.ID, Backend: a.Backend, Version: a.Version, SessionID: sid, Status: "starting", Expires: time.Now().Add(10 * time.Minute), cancel: cancel}
	cmd := exec.CommandContext(ctx, r.Config.RcloneBin, "authorize", rcloneType(a.Backend), "--auth-no-open-browser", "--config", path, "--log-level", "NOTICE")
	cmd.Env = rcloneEnvironment()
	out := &privateOutput{}
	cmd.Stdout = out
	stderr, e := cmd.StderrPipe()
	if e == nil {
		e = cmd.Start()
	}
	if e != nil {
		cancel()
		os.Remove(path)
		r.rcloneAuthMu.Unlock()
		fail(w, 500, "authorization_start_failed", "无法启动 Rclone 标准授权")
		return
	}
	r.rcloneAuth[f.ID] = f
	r.rcloneAuthWG.Add(1)
	r.rcloneAuthMu.Unlock()
	go r.runRcloneAuth(ctx, f, cmd, stderr, out, path)
	writeJSON(w, 202, map[string]any{"id": f.ID, "status": "starting", "expires_at": f.Expires})
}
func (r *Runtime) runRcloneAuth(ctx context.Context, f *rcloneAuthFlow, cmd *exec.Cmd, stderr io.Reader, out *privateOutput, path string) {
	defer r.rcloneAuthWG.Done()
	defer f.cancel()
	defer os.Remove(path)
	scan := bufio.NewScanner(stderr)
	scan.Buffer(make([]byte, 4096), maxRcloneImport)
	for scan.Scan() {
		link := localAuthLink.FindString(scan.Text())
		if link == "" {
			continue
		}
		u, _ := url.Parse(link)
		state := u.Query().Get("state")
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		request, _ := http.NewRequestWithContext(ctx, "GET", link, nil)
		res, e := client.Do(request)
		if e != nil {
			f.cancel()
			continue
		}
		location := res.Header.Get("Location")
		res.Body.Close()
		client.CloseIdleConnections()
		auth, e := validatedRcloneAuthURL(location, state, f.Backend)
		if e != nil || res.StatusCode != http.StatusTemporaryRedirect {
			f.cancel()
			continue
		}
		r.rcloneAuthMu.Lock()
		if f.Status == "starting" {
			f.State = state
			f.AuthorizationURL = auth.String()
			f.Status = "waiting"
		}
		r.rcloneAuthMu.Unlock()
	}
	if scan.Err() != nil {
		f.cancel()
	}
	e := cmd.Wait()
	_ = os.Remove(path)
	r.rcloneAuthMu.Lock()
	defer r.rcloneAuthMu.Unlock()
	defer func() {
		if f.Status == "failed" {
			r.AccountAuthorizationFailed(f.AccountID, "authorization_failed")
		}
	}()
	f.AuthorizationURL = ""
	f.State = ""
	if f.Status == "cancelling" || r.closing {
		f.Status = "cancelled"
		f.Message = "授权已取消"
		return
	}
	if ctx.Err() != nil {
		f.Status = "failed"
		f.Message = "授权已过期或启动失败，请检查 53682 端口后重试"
		return
	}
	if e != nil {
		f.Status = "failed"
		provider := "微软"
		if f.Backend == backendGoogleDrive {
			provider = "Google"
		}
		f.Message = "Rclone 授权未完成，请检查端口、网络或" + provider + "授权结果后重试"
		return
	}
	entries, e := parseRcloneImport(out.String(), f.Backend)
	if e != nil || len(entries) != 1 {
		f.Status = "failed"
		f.Message = "Rclone 未返回有效授权凭据"
		return
	}
	a, e := r.installCredential(ctx, f.AccountID, f.Version, entries[0].Credential, "account.rclone_authorized")
	if e != nil {
		f.Status = "failed"
		f.Message = e.Error()
		return
	}
	f.Status = "completed"
	f.Account = &a
	f.Message = "授权凭据已加密保存，请选择 Drive 并验证访问"
}

// Validate the provider destination before exposing a URL from the local
// Rclone process. Callback relaying is restricted separately to loopback.
func validatedRcloneAuthURL(location, state, backend string) (*url.URL, error) {
	bad := errors.New("Rclone 返回了不受支持的授权地址")
	auth, err := url.Parse(location)
	if err != nil || auth.Scheme != "https" || auth.User != nil || auth.Query().Get("state") != state {
		return nil, bad
	}
	q := auth.Query()
	if backend == backendGoogleDrive {
		if auth.Host != "accounts.google.com" || auth.Path != "/o/oauth2/auth" || auth.Fragment != "" {
			return nil, bad
		}
		q, err = url.ParseQuery(auth.RawQuery)
		if err != nil {
			return nil, bad
		}
		for key, value := range map[string]string{"state": state, "redirect_uri": "http://127.0.0.1:53682/", "response_type": "code", "access_type": "offline", "scope": "https://www.googleapis.com/auth/drive"} {
			if len(q[key]) != 1 || q.Get(key) != value {
				return nil, bad
			}
		}
		if len(q["client_id"]) != 1 || !strings.HasSuffix(q.Get("client_id"), ".apps.googleusercontent.com") {
			return nil, bad
		}
		q.Set("prompt", "consent select_account")
	} else {
		if auth.Host != "login.microsoftonline.com" || auth.Path != "/common/oauth2/v2.0/authorize" || q.Get("redirect_uri") != "http://localhost:53682/" {
			return nil, bad
		}
		q.Set("prompt", "select_account")
	}
	auth.RawQuery = q.Encode()
	return auth, nil
}

func (r *Runtime) ownedRcloneFlow(req *http.Request) (*rcloneAuthFlow, bool) {
	_, sid, ok := r.session(req)
	f := r.rcloneAuth[req.URL.Query().Get("flow_id")]
	return f, ok && f != nil && f.AccountID == req.PathValue("id") && f.SessionID == sid
}
func (r *Runtime) getRcloneAuth(w http.ResponseWriter, req *http.Request) {
	r.rcloneAuthMu.Lock()
	defer r.rcloneAuthMu.Unlock()
	f, ok := r.ownedRcloneFlow(req)
	if !ok {
		fail(w, 404, "authorization_not_found", "授权会话不存在，请重新开始")
		return
	}
	writeJSON(w, 200, f.view())
}
func (r *Runtime) cancelRcloneAuth(w http.ResponseWriter, req *http.Request) {
	r.rcloneAuthMu.Lock()
	defer r.rcloneAuthMu.Unlock()
	f, ok := r.ownedRcloneFlow(req)
	if !ok {
		fail(w, 404, "authorization_not_found", "授权会话不存在")
		return
	}
	if f.active() {
		f.Status = "cancelling"
		f.AuthorizationURL = ""
		f.cancel()
	}
	writeJSON(w, 200, f.view())
}
func validateRcloneCallback(raw, state, backend string) (url.Values, error) {
	message := "请粘贴本次微软授权返回的完整 http://localhost:53682/ 地址（含 code 与 state）"
	if backend == backendGoogleDrive {
		message = "请粘贴本次 Google 授权返回的完整 http://127.0.0.1:53682/ 地址（含 code 与 state）"
	}
	bad := errors.New(message)
	if len(raw) > 65536 {
		return nil, bad
	}
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil || u.Scheme != "http" || (u.Host != "localhost:53682" && u.Host != "127.0.0.1:53682") || u.Path != "/" || u.User != nil || u.Fragment != "" {
		return nil, bad
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil || state == "" || q.Get("state") != state || len(q["state"]) != 1 || len(q["code"]) > 1 || len(q["error"]) > 1 {
		return nil, bad
	}
	if (q.Get("code") == "") == (q.Get("error") == "") {
		return nil, bad
	}
	values := url.Values{"state": {state}}
	if q.Get("code") != "" {
		values.Set("code", q.Get("code"))
	} else {
		values.Set("error", q.Get("error"))
	}
	return values, nil
}
func (r *Runtime) relayRcloneCallback(w http.ResponseWriter, req *http.Request) {
	var v struct {
		URL string `json:"url"`
	}
	if !decode(w, req, &v) {
		return
	}
	r.rcloneAuthMu.Lock()
	f, ok := r.ownedRcloneFlow(req)
	if !ok {
		r.rcloneAuthMu.Unlock()
		fail(w, 404, "authorization_not_found", "授权会话不存在")
		return
	}
	if f.Status != "waiting" || time.Now().After(f.Expires) {
		r.rcloneAuthMu.Unlock()
		fail(w, 409, "authorization_not_waiting", "授权已结束或正在交换凭据，请查看状态")
		return
	}
	q, e := validateRcloneCallback(v.URL, f.State, f.Backend)
	if e != nil {
		r.rcloneAuthMu.Unlock()
		fail(w, 400, "invalid_callback_url", e.Error())
		return
	}
	f.Status = "exchanging"
	r.rcloneAuthMu.Unlock()
	// Never request the pasted destination: construct a fixed loopback URL.
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:53682/?"+q.Encode(), nil)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, e := client.Do(request)
	if res != nil {
		res.Body.Close()
	}
	client.CloseIdleConnections()
	if e != nil {
		r.rcloneAuthMu.Lock()
		if f.Status == "exchanging" {
			f.Status = "cancelling"
			f.cancel()
		}
		r.rcloneAuthMu.Unlock()
		fail(w, 502, "callback_delivery_failed", "无法提交回调，请取消后重新授权")
		return
	}
	writeJSON(w, 202, map[string]string{"status": "exchanging"})
}
