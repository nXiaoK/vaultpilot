package platform

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestUpdateAccountNameMatchesCreateLimits(t *testing.T) {
	h := newPendingCloudHarness(t, adminPassword)
	res := h.call("POST", "/api/accounts", map[string]any{"name": "Family"}, true)
	if res.Code != 201 {
		t.Fatal(res.Body.String())
	}
	var a Account
	if err := json.Unmarshal(res.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"empty", "  ", false},
		{"ascii_boundary", strings.Repeat("a", 200), true},
		{"ascii_too_long", strings.Repeat("a", 201), false},
		{"utf8_within_byte_limit", strings.Repeat("中", 66), true},
		{"utf8_over_byte_limit", strings.Repeat("中", 67), false},
		{"trimmed_boundary", " " + strings.Repeat("a", 200) + " ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			create := h.call("POST", "/api/accounts", map[string]any{"name": tc.value}, true)
			res := h.call("PUT", "/api/accounts/"+a.ID, map[string]any{"name": tc.value, "version": a.Version, "budget_bytes": a.BudgetBytes, "cloud_reserve_bytes": a.CloudReserveBytes, "maintenance_reserve_bytes": a.MaintenanceReserveBytes}, true)
			if tc.valid {
				if create.Code != 201 || res.Code != 200 {
					t.Fatalf("valid boundary rejected: create=%d update=%d %s", create.Code, res.Code, res.Body.String())
				}
				if err := json.Unmarshal(res.Body.Bytes(), &a); err != nil {
					t.Fatal(err)
				}
				if a.Name != strings.TrimSpace(tc.value) {
					t.Fatalf("name was not trimmed: %q", a.Name)
				}
			} else {
				if create.Code != 400 || res.Code != 400 {
					t.Fatalf("invalid name accepted: create=%d update=%d %s", create.Code, res.Code, res.Body.String())
				}
				var persisted Account
				if err := h.r.Store.Get(t.Context(), "accounts", a.ID, &persisted); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(a, persisted) {
					t.Fatalf("invalid update modified account: %+v", persisted)
				}
			}
		})
	}
}
