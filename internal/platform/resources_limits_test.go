package platform

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheBudgetUsesRealFilesWithoutDeletingActiveCache(t *testing.T) {
	h := newHarness(t, false)
	h.r.Config.CacheMaxBytes = 10
	path := filepath.Join(h.r.Config.CacheDir, "active-restic-cache")
	if err := os.WriteFile(path, []byte("1234567890"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.r.CheckCacheBudget(); err != nil {
		t.Fatal("exact budget rejected", err)
	}
	if err := os.WriteFile(path, []byte("12345678901"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.r.CheckCacheBudget(); err == nil {
		t.Fatal("over-budget real cache accepted")
	}
	if data, err := os.ReadFile(path); err != nil || len(data) != 11 {
		t.Fatal("active cache was modified")
	}
}

func TestMaintenanceWindowTimezoneAndMidnightBoundaries(t *testing.T) {
	r := &Runtime{Config: Config{MaintenanceWindow: "23:00-02:00", MaintenanceTimezone: "Asia/Shanghai"}}
	for _, sample := range []struct {
		utc     string
		allowed bool
	}{{"2026-10-10T14:59:00Z", false}, {"2026-10-10T15:00:00Z", true}, {"2026-10-10T17:59:00Z", true}, {"2026-10-10T18:00:00Z", false}} {
		at, _ := time.Parse(time.RFC3339, sample.utc)
		allowed, err := r.AllowedMaintenance(at)
		if err != nil || allowed != sample.allowed {
			t.Fatalf("%s allowed=%v err=%v", sample.utc, allowed, err)
		}
	}
	r.Config.MaintenanceWindow = "09:30-12:00"
	at, _ := time.Parse(time.RFC3339, "2026-10-10T01:30:00Z")
	if ok, e := r.AllowedMaintenance(at); e != nil || !ok {
		t.Fatal("timezone daily window failed")
	}
	for _, bad := range []string{"24:00-02:00", "1:00-02:00", "23:99-02:00", "02:00-02:00", "23:00 - 02:00"} {
		r.Config.MaintenanceWindow = bad
		if _, e := r.AllowedMaintenance(at); e == nil {
			t.Fatal("invalid maintenance window accepted", bad)
		}
	}
	r.Config.MaintenanceWindow = ""
	if ok, e := r.AllowedMaintenance(at); e != nil || !ok {
		t.Fatal("empty window restricted maintenance")
	}
	r.Config.MaintenanceTimezone = "invalid/no-zone"
	if _, e := r.AllowedMaintenance(at); e == nil {
		t.Fatal("invalid timezone accepted")
	}
}
