package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnreadableSourceExplainsReadOnlyGrant(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	h := newHarness(t, false)
	locked := filepath.Join(h.r.Config.SourceRoots[0], "locked")
	if e := os.Mkdir(locked, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(locked, 0); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	_, e := h.r.ValidateSourcePath(locked)
	if e == nil || !strings.Contains(e.Error(), fmt.Sprintf("UID=%d", os.Geteuid())) || !strings.Contains(e.Error(), "不要用 chown/chmod") {
		t.Fatalf("permission error lacks read-only guidance: %v", e)
	}
	// Validation only reads; the owning application's mode must stay as is.
	if info, e := os.Stat(locked); e != nil || info.Mode().Perm() != 0 {
		t.Fatalf("source permissions changed: %v %v", info, e)
	}
	if hint := SourceAccessHint(os.ErrNotExist); hint != "" {
		t.Fatalf("non-permission errors must keep the plain message: %q", hint)
	}
}
