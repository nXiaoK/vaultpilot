package platform

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestAccountIdentityAndUniqueDrives(t *testing.T) {
	for _, profileAvailable := range []bool{true, false} {
		h := newPendingCloudHarness(t, adminPassword)
		res := h.call("POST", "/api/accounts", map[string]any{"name": "Family", "account_email": "forged@example.com"}, true)
		var a Account
		json.Unmarshal(res.Body.Bytes(), &a)
		if a.AccountEmail != "" {
			t.Fatal("accepted caller identity")
		}
		h.r.saveToken(a.ID, OAuthToken{AccessToken: "test-access", RefreshToken: "test-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})
		h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/v1.0/me/drives" {
				return cloudResponse(200, `{"value":[{"id":"same","name":"Bundles_metadata","driveType":"personal"},{"id":"same","name":"OneDrive","driveType":"personal"}]}`), nil
			}
			if profileAvailable {
				return cloudResponse(200, `{"displayName":"Family member","mail":"member@outlook.com"}`), nil
			}
			return cloudResponse(403, `{}`), nil
		})}
		res = h.call("GET", "/api/accounts/"+a.ID+"/drives", nil, false)
		if res.Code != 200 {
			t.Fatal(res.Body.String())
		}
		var result struct {
			Items   []Drive
			Account Account
		}
		json.Unmarshal(res.Body.Bytes(), &result)
		if len(result.Items) != 1 || result.Items[0].Name != "OneDrive" {
			t.Fatalf("duplicate drive: %+v", result.Items)
		}
		if profileAvailable {
			if result.Account.AccountEmail != "member@outlook.com" || result.Account.Version != a.Version+1 {
				t.Fatalf("identity: %+v", result.Account)
			}
			_, err := h.r.installCredential(t.Context(), a.ID, result.Account.Version, h.r.customCredential(OAuthToken{AccessToken: "new"}), "test")
			if err != nil {
				t.Fatal(err)
			}
			h.r.Store.Get(t.Context(), "accounts", a.ID, &a)
			if a.AccountEmail != "" || a.AccountDisplayName != "" {
				t.Fatal("retained identity on reauthorization")
			}
		} else if result.Account.Version != a.Version {
			t.Fatal("optional profile failure mutated account")
		}
	}
}
