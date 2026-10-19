package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGoogleStorageGroupInputAndProviderIsolation(t *testing.T) {
	h := newPendingCloudHarness(t, adminPassword)
	for _, test := range []struct {
		backend, input, want string
		status               int
	}{
		{"gdrive", "  家庭存储  ", "家庭存储", 201},
		{"gdrive", strings.Repeat("家", 64), strings.Repeat("家", 64), 201},
		{"gdrive", strings.Repeat("家", 65), "", 400},
		{"onedrive", "家庭存储", "", 201},
	} {
		res := h.call("POST", "/api/accounts", map[string]any{"name": "storage", "backend": test.backend, "storage_group": test.input}, true)
		if res.Code != test.status {
			t.Fatalf("create %s: %d %s", test.backend, res.Code, res.Body.String())
		}
		if test.status != 201 {
			continue
		}
		var a Account
		json.Unmarshal(res.Body.Bytes(), &a)
		if a.StorageGroup != test.want {
			t.Fatalf("group not normalized: %q", a.StorageGroup)
		}
		res = h.call("PUT", "/api/accounts/"+a.ID, map[string]any{"version": a.Version, "name": a.Name, "budget_bytes": a.BudgetBytes, "storage_group": "  新家庭  "}, true)
		if res.Code != 200 {
			t.Fatal(res.Body.String())
		}
		json.Unmarshal(res.Body.Bytes(), &a)
		want := "新家庭"
		if test.backend != "gdrive" {
			want = ""
		}
		if a.StorageGroup != want {
			t.Fatalf("updated provider group: %q", a.StorageGroup)
		}
	}
}

func TestGoogleStorageGroupPoolAndExistingPoolEdits(t *testing.T) {
	h := newPendingCloudHarness(t, adminPassword)
	for _, test := range []struct {
		groups  []string
		allowed bool
	}{
		{[]string{"家庭", "家庭"}, false},
		{[]string{"家庭", "另一个家庭"}, true},
		{[]string{"", ""}, true},
		{[]string{"家庭", ""}, true},
	} {
		accounts := []Account{}
		for _, group := range test.groups {
			a := googleTestAccount(t, h)
			a.StorageGroup, a.Status, a.Verified = group, "ready", true
			a.QuotaDomain = "google:personal:" + a.ID
			version, err := h.r.Store.Save(t.Context(), "accounts", a.ID, a.Version, a)
			if err != nil {
				t.Fatal(err)
			}
			a.Version = version
			accounts = append(accounts, a)
		}
		res := h.call("POST", "/api/pools", map[string]any{"name": "mixed", "account_ids": []string{accounts[0].ID, accounts[1].ID}}, true)
		if !test.allowed {
			if res.Code != 409 || !strings.Contains(res.Body.String(), "shared_storage_group") {
				t.Fatalf("shared pool accepted: %d %s", res.Code, res.Body.String())
			}
			continue
		}
		if res.Code != 201 {
			t.Fatal(res.Body.String())
		}
		if accounts[0].StorageGroup == "" {
			continue
		}
		a := accounts[1]
		res = h.call("PUT", "/api/accounts/"+a.ID, map[string]any{"name": a.Name, "version": a.Version, "budget_bytes": a.BudgetBytes, "storage_group": "  " + accounts[0].StorageGroup + "  "}, true)
		if res.Code != 409 || !strings.Contains(res.Body.String(), "shared_storage_group") {
			t.Fatalf("edit bypassed existing pool group: %d %s", res.Code, res.Body.String())
		}
		var current Account
		if err := h.r.Store.Get(t.Context(), "accounts", a.ID, &current); err != nil || current.StorageGroup != a.StorageGroup || current.Version != a.Version {
			t.Fatalf("rejected edit changed account: %+v %v", current, err)
		}
	}
}
