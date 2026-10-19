package platform

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCloudProvidersPreserveOneDriveErrors(t *testing.T) {
	for _, tc := range []struct {
		status   int
		message  string
		code     string
		response int
	}{
		{429, "微软接口限流，请稍后重试", "cloud_throttled", 429},
		{401, "授权已失效或未授予所需权限，请重新授权", "authorization_required", 409},
		{403, "授权已失效或未授予所需权限，请重新授权", "authorization_required", 409},
		{500, "微软接口请求失败（HTTP 500）", "cloud_access_failed", 502},
	} {
		err := cloudError{Status: tc.status, RetryAfter: "12"}
		if err.Error() != tc.message {
			t.Fatalf("status %d: %s", tc.status, err.Error())
		}
		w := httptest.NewRecorder()
		cloudFailure(w, err)
		if w.Code != tc.response || !strings.Contains(w.Body.String(), tc.code) || w.Header().Get("Retry-After") != "12" {
			t.Fatalf("status %d: %d %s", tc.status, w.Code, w.Body.String())
		}
	}
	if !cloudBackend("onedrive") || !cloudBackend("gdrive") || cloudBackend("local") || rcloneType("gdrive") != "drive" || backendLabel("gdrive") != "Google Drive" {
		t.Fatal("unexpected provider mapping")
	}
}
