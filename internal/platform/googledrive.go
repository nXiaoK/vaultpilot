package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const driveAPIBase = "https://www.googleapis.com/drive/v3"
const driveUploadBase = "https://www.googleapis.com/upload/drive/v3"
const googleFolderMIME = "application/vnd.google-apps.folder"
const googleUnknownQuota = "Google 未返回容量上限（不限容量或组织共享池），禁止把未知空间视为无限容量"

var googleFolderName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type duplicateCloudFolder struct{}

func (duplicateCloudFolder) Error() string {
	return "Google Drive 存在同名项目目录，请在 Google Drive 中手动处理重复目录后重试"
}

func (r *Runtime) gdrive(ctx context.Context, id, method, path string, query url.Values, body []byte, contentType string) ([]byte, error) {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") {
		return nil, errors.New("无效 Google Drive 路径")
	}
	base := driveAPIBase
	if strings.HasPrefix(path, "/upload/") {
		base, path = driveUploadBase, strings.TrimPrefix(path, "/upload")
	}
	t, err := r.accessToken(ctx, id)
	if err != nil {
		return nil, err
	}
	endpoint := base + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.AccessToken)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("无法连接 Google Drive")
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
	if err != nil {
		return nil, errors.New("读取 Google Drive 响应失败")
	}
	if len(b) > 8<<20 {
		return nil, errors.New("Google Drive 响应过大")
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var failure struct {
			Error struct {
				Errors []struct{ Reason string } `json:"errors"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &failure)
		cloud := cloudError{Status: res.StatusCode, RetryAfter: res.Header.Get("Retry-After"), Provider: backendGoogleDrive}
		for _, detail := range failure.Error.Errors {
			if cloud.Reason == "" {
				cloud.Reason = detail.Reason
			}
			// Capacity and throttling failures must not become authorization alerts,
			// even when Google includes more than one error reason.
			switch detail.Reason {
			case "rateLimitExceeded", "userRateLimitExceeded", "dailyLimitExceeded", "storageQuotaExceeded":
				cloud.Reason = detail.Reason
			case "insufficientPermissions", "authError":
				if !cloud.throttled() && cloud.Reason != "storageQuotaExceeded" {
					cloud.Reason = detail.Reason
				}
			}
		}
		if strings.HasPrefix(path, "/files") && cloud.authorizationFailure() {
			r.ObserveAccountAccess(id, cloud)
		}
		return nil, cloud
	}
	return b, nil
}

type googleAbout struct {
	User struct {
		DisplayName  string `json:"displayName"`
		EmailAddress string `json:"emailAddress"`
		PermissionID string `json:"permissionId"`
	} `json:"user"`
	StorageQuota struct {
		Limit *string `json:"limit"`
		Usage *string `json:"usage"`
	} `json:"storageQuota"`
}

func (r *Runtime) googleListDrives(ctx context.Context, id string) ([]Drive, *accountProfile, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type response struct {
		body []byte
		err  error
	}
	aboutResult := make(chan response, 1)
	go func() {
		b, err := r.gdrive(ctx, id, http.MethodGet, "/about", url.Values{"fields": {"user(displayName,emailAddress,permissionId),storageQuota(limit,usage,usageInDrive,usageInDriveTrash)"}}, nil, "")
		aboutResult <- response{b, err}
	}()
	b, err := r.gdrive(ctx, id, http.MethodGet, "/files/root", url.Values{"fields": {"id"}}, nil, "")
	if err != nil {
		return nil, nil, err
	}
	var root struct{ ID string }
	if json.Unmarshal(b, &root) != nil || root.ID == "" {
		return nil, nil, errors.New("Google Drive 根目录响应无效")
	}
	var result response
	select {
	case result = <-aboutResult:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	if result.err != nil {
		return nil, nil, result.err
	}
	var about googleAbout
	if json.Unmarshal(result.body, &about) != nil {
		return nil, nil, errors.New("Google Drive 账号响应无效")
	}
	d := Drive{ID: root.ID, Name: "我的云端硬盘", DriveType: "my_drive", Supported: about.StorageQuota.Limit != nil}
	d.Owner.User.Email, d.Owner.User.DisplayName = about.User.EmailAddress, about.User.DisplayName
	if !d.Supported {
		d.UnsupportedReason = googleUnknownQuota
	} else {
		total, err := strconv.ParseInt(*about.StorageQuota.Limit, 10, 64)
		if err != nil || total < 0 {
			return nil, nil, errors.New("Google Drive 配额响应无效")
		}
		d.Quota.Total = &total
		if about.StorageQuota.Usage != nil {
			used, err := strconv.ParseInt(*about.StorageQuota.Usage, 10, 64)
			if err != nil || used < 0 {
				return nil, nil, errors.New("Google Drive 配额响应无效")
			}
			free := max(int64(0), total-used)
			d.Quota.Used, d.Quota.Remaining = &used, &free
		}
	}
	profile := &accountProfile{Email: about.User.EmailAddress, DisplayName: about.User.DisplayName, Expires: time.Now().Add(driveListTTL)}
	return []Drive{d}, profile, nil
}

func (r *Runtime) googleFindChild(ctx context.Context, a Account, parentID, name string) (string, error) {
	if !googleFolderName.MatchString(name) {
		return "", errors.New("Google Drive 项目目录名称无效")
	}
	parent := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(parentID)
	query := url.Values{"q": {"'" + parent + "' in parents and name = '" + name + "' and trashed = false"}, "fields": {"nextPageToken,files(id,mimeType)"}, "pageSize": {"10"}, "spaces": {"drive"}}
	var found []struct{ ID, MimeType string }
	seenPages := map[string]bool{}
	for {
		b, err := r.gdrive(ctx, a.ID, http.MethodGet, "/files", query, nil, "")
		if err != nil {
			return "", err
		}
		var page struct {
			Files         []struct{ ID, MimeType string }
			NextPageToken string
		}
		if json.Unmarshal(b, &page) != nil {
			return "", errors.New("Google Drive 目录响应无效")
		}
		found = append(found, page.Files...)
		if len(found) > 1 {
			return "", duplicateCloudFolder{}
		}
		if page.NextPageToken == "" {
			break
		}
		if seenPages[page.NextPageToken] {
			return "", errors.New("Google Drive 目录分页响应无效")
		}
		seenPages[page.NextPageToken] = true
		query.Set("pageToken", page.NextPageToken)
	}
	if len(found) == 0 {
		return "", nil
	}
	if found[0].ID == "" || found[0].MimeType != googleFolderMIME {
		return "", errors.New("项目目录被非目录项目占用")
	}
	return found[0].ID, nil
}

func (r *Runtime) googleEnsureFolder(ctx context.Context, a Account, parentID, name string) (string, error) {
	id, err := r.googleFindChild(ctx, a, parentID, name)
	if err != nil || id != "" {
		return id, err
	}
	body, _ := json.Marshal(map[string]any{"name": name, "mimeType": googleFolderMIME, "parents": []string{parentID}})
	b, err := r.gdrive(ctx, a.ID, http.MethodPost, "/files", url.Values{"fields": {"id"}}, body, "application/json")
	if err != nil {
		return "", err
	}
	var created struct{ ID string }
	if json.Unmarshal(b, &created) != nil || created.ID == "" {
		return "", errors.New("创建 Google Drive 项目目录响应无效")
	}
	id, err = r.googleFindChild(ctx, a, parentID, name)
	var duplicate duplicateCloudFolder
	if errors.As(err, &duplicate) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		parent := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(created.ID)
		b, checkErr := r.gdrive(cleanupCtx, a.ID, http.MethodGet, "/files", url.Values{"q": {"'" + parent + "' in parents and trashed = false"}, "fields": {"nextPageToken,files(id)"}, "pageSize": {"1"}, "spaces": {"drive"}}, nil, "")
		var children struct {
			Files         []json.RawMessage
			NextPageToken string
		}
		if checkErr == nil && json.Unmarshal(b, &children) == nil && len(children.Files) == 0 && children.NextPageToken == "" {
			_, _ = r.gdrive(cleanupCtx, a.ID, http.MethodDelete, "/files/"+url.PathEscape(created.ID), nil, nil, "")
		}
	}
	if err == nil && id != created.ID {
		return "", errors.New("Google Drive 新建目录未能确认，请重试")
	}
	return id, err
}

func (r *Runtime) googleVerifyCloud(ctx context.Context, a *Account) error {
	if err := r.googleCheckAccess(ctx, *a); err != nil {
		return err
	}
	if r.RecoveryMode() {
		root, err := r.googleFindChild(ctx, *a, a.DriveID, "VaultPilot")
		if err != nil {
			return err
		}
		if root == "" {
			return errors.New("Google Drive 中不存在 VaultPilot 备份目录")
		}
	} else {
		root, err := r.googleEnsureFolder(ctx, *a, a.DriveID, "VaultPilot")
		if err != nil {
			return err
		}
		vault, err := r.googleEnsureFolder(ctx, *a, root, r.VaultID())
		if err != nil {
			return err
		}
		content := randomBytes(48)
		metadata, _ := json.Marshal(map[string]any{"name": ".verification-" + newID() + ".bin", "parents": []string{vault}})
		var upload bytes.Buffer
		writer := multipart.NewWriter(&upload)
		part, _ := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
		_, _ = part.Write(metadata)
		part, _ = writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
		_, _ = part.Write(content)
		_ = writer.Close()
		b, err := r.gdrive(ctx, a.ID, http.MethodPost, "/upload/files", url.Values{"uploadType": {"multipart"}, "fields": {"id"}}, upload.Bytes(), "multipart/related; boundary="+writer.Boundary())
		if err != nil {
			return err
		}
		var file struct{ ID string }
		if json.Unmarshal(b, &file) != nil || file.ID == "" {
			return errors.New("Google Drive 写入验证响应无效")
		}
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_, _ = r.gdrive(cleanupCtx, a.ID, http.MethodDelete, "/files/"+url.PathEscape(file.ID), nil, nil, "")
		}()
		read, err := r.gdrive(ctx, a.ID, http.MethodGet, "/files/"+url.PathEscape(file.ID), url.Values{"alt": {"media"}}, nil, "")
		if err != nil {
			return err
		}
		if !bytes.Equal(read, content) {
			return errors.New("Google Drive 写入后读回验证失败")
		}
	}
	if err := r.googleQuota(ctx, a); err != nil {
		return err
	}
	a.Verified, a.Status = true, "ready"
	return nil
}

func (r *Runtime) googleQuota(ctx context.Context, a *Account) error {
	b, err := r.gdrive(ctx, a.ID, http.MethodGet, "/about", url.Values{"fields": {"user(displayName,emailAddress),storageQuota(limit,usage)"}}, nil, "")
	if err != nil {
		return err
	}
	var about googleAbout
	if json.Unmarshal(b, &about) != nil {
		return errors.New("Google Drive 配额响应无效")
	}
	if about.StorageQuota.Limit == nil {
		return errors.New(googleUnknownQuota)
	}
	total, err := strconv.ParseInt(*about.StorageQuota.Limit, 10, 64)
	if err != nil || total < 0 || about.StorageQuota.Usage == nil {
		return errors.New("Google Drive 配额响应无效")
	}
	used, err := strconv.ParseInt(*about.StorageQuota.Usage, 10, 64)
	if err != nil || used < 0 {
		return errors.New("Google Drive 配额响应无效")
	}
	root, err := r.googleFindChild(ctx, *a, a.DriveID, "VaultPilot")
	if err != nil {
		return err
	}
	if root != "" && r.VaultID() != "" {
		root, err = r.googleFindChild(ctx, *a, root, r.VaultID())
		if err != nil {
			return err
		}
	}
	projectUsed := int64(0)
	queue := []string{}
	if root != "" {
		queue = append(queue, root)
	}
	seen := map[string]bool{}
	for len(queue) > 0 {
		folder := queue[0]
		queue = queue[1:]
		if seen[folder] {
			continue
		}
		seen[folder] = true
		parent := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(folder)
		query := url.Values{"q": {"'" + parent + "' in parents and trashed = false"}, "fields": {"nextPageToken,files(id,mimeType,size)"}, "pageSize": {"1000"}, "spaces": {"drive"}}
		pages := map[string]bool{}
		for {
			b, err := r.gdrive(ctx, a.ID, http.MethodGet, "/files", query, nil, "")
			if err != nil {
				return err
			}
			var page struct {
				NextPageToken string
				Files         []struct {
					ID, MimeType string
					Size         *string
				}
			}
			if json.Unmarshal(b, &page) != nil {
				return errors.New("Google Drive 项目占用响应无效")
			}
			for _, file := range page.Files {
				if file.ID == "" {
					return errors.New("Google Drive 项目占用响应无效")
				}
				if file.MimeType == googleFolderMIME {
					queue = append(queue, file.ID)
					continue
				}
				// Native Google documents may omit size and consume no binary bytes.
				if file.Size == nil {
					continue
				}
				size, err := strconv.ParseInt(*file.Size, 10, 64)
				if err != nil || size < 0 || size > math.MaxInt64-projectUsed {
					return errors.New("Google Drive 项目占用响应无效")
				}
				projectUsed += size
			}
			if page.NextPageToken == "" {
				break
			}
			if pages[page.NextPageToken] {
				return errors.New("Google Drive 项目占用分页响应无效")
			}
			pages[page.NextPageToken] = true
			query.Set("pageToken", page.NextPageToken)
		}
	}
	a.TotalBytes, a.FreeBytes = total, max(int64(0), total-used)
	a.UsedBytes = total - a.FreeBytes
	a.ProjectUsedBytes = projectUsed
	if about.User.EmailAddress != "" {
		a.AccountEmail = about.User.EmailAddress
	}
	if about.User.DisplayName != "" {
		a.AccountDisplayName = about.User.DisplayName
	}
	a.QuotaAt = time.Now().UTC().Format(time.RFC3339)
	return nil
}

func (r *Runtime) googleCheckAccess(ctx context.Context, a Account) error {
	b, err := r.gdrive(ctx, a.ID, http.MethodGet, "/files/"+url.PathEscape(a.DriveID), url.Values{"fields": {"id"}}, nil, "")
	if err != nil {
		return err
	}
	var root struct{ ID string }
	if json.Unmarshal(b, &root) != nil || root.ID == "" || root.ID != a.DriveID {
		return errors.New("Google Drive 根目录身份不一致，请重新授权")
	}
	return nil
}
