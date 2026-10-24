package platform

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestRcloneArgsForScopesGoogleDriveLimiter(t *testing.T) {
	r := &Runtime{Config: Config{GoogleDriveTPSLimit: 4, GoogleDriveTPSLimitBurst: 3}}
	base := []string{"--config", "/tmp/rclone.conf", "about", "drive:"}
	for _, tc := range []struct {
		name     string
		accounts []Account
		want     []string
	}{
		{name: "google", accounts: []Account{{Backend: backendGoogleDrive}}, want: []string{"--tpslimit", "4", "--tpslimit-burst", "3"}},
		{name: "google and local", accounts: []Account{{Backend: backendGoogleDrive}, {Backend: "local"}}, want: []string{"--tpslimit", "4", "--tpslimit-burst", "3"}},
		{name: "onedrive", accounts: []Account{{Backend: backendOneDrive}}, want: nil},
		{name: "local", accounts: []Account{{Backend: "local"}}, want: nil},
		{name: "mixed cloud", accounts: []Account{{Backend: backendGoogleDrive}, {Backend: backendOneDrive}}, want: nil},
		{name: "empty", accounts: nil, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := append([]string(nil), base...)
			got := r.RcloneArgsFor(tc.accounts, input...)
			if !reflect.DeepEqual(input, base) {
				t.Fatalf("helper mutated input: %v", input)
			}
			for _, value := range tc.want {
				if !containsArgPair(got, value) && value != "--tpslimit" && value != "--tpslimit-burst" {
					t.Fatalf("missing %q in %v", value, got)
				}
			}
			if len(tc.want) == 0 {
				if !reflect.DeepEqual(got, base) {
					t.Fatalf("unexpected limiter for %s: %v", tc.name, got)
				}
				return
			}
			if !hasArgs(got, "--tpslimit", "4", "--tpslimit-burst", "3") {
				t.Fatalf("missing limiter args: %v", got)
			}
		})
	}

	r.Config.GoogleDriveTPSLimit = 0
	if got := r.RcloneArgsFor([]Account{{Backend: backendGoogleDrive}}, base...); !reflect.DeepEqual(got, base) {
		t.Fatalf("zero limit was not disabled: %v", got)
	}
}

func TestGoogleDriveCredentialConfigUsesBackendPacerOnly(t *testing.T) {
	google := &Runtime{Config: Config{GoogleDriveTPSLimit: 4, GoogleDriveTPSLimitBurst: 2}}
	credential := accountCredential{Provider: backendGoogleDrive, Mode: "rclone", Token: OAuthToken{AccessToken: "access"}}
	content, err := google.credentialConfig(context.Background(), "drive", credential, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "pacer_min_sleep = 250ms\n") || !strings.Contains(content, "pacer_burst = 2\n") {
		t.Fatalf("Google config did not receive backend pacer: %s", content)
	}

	onedrive := &Runtime{Config: google.Config}
	content, err = onedrive.credentialConfig(context.Background(), "onedrive", accountCredential{Mode: "custom", Token: OAuthToken{AccessToken: "access"}}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "pacer_min_sleep") || strings.Contains(content, "pacer_burst") {
		t.Fatalf("OneDrive config received Google pacer: %s", content)
	}
}

func TestRuntimeRejectsInvalidGoogleDriveRateLimit(t *testing.T) {
	for _, limit := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 0.009, 1000.001} {
		t.Run("limit", func(t *testing.T) {
			if _, err := New(Config{GoogleDriveTPSLimit: limit, GoogleDriveTPSLimitBurst: 1}); err == nil {
				t.Fatalf("invalid limit accepted: %v", limit)
			}
		})
	}
	for _, burst := range []int{-1, 101} {
		if _, err := New(Config{GoogleDriveTPSLimit: 4, GoogleDriveTPSLimitBurst: burst}); err == nil {
			t.Fatalf("invalid burst accepted: %d", burst)
		}
	}
}

func TestRuntimeZeroRateLimitNormalizesBurst(t *testing.T) {
	r, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Config.GoogleDriveTPSLimit != 0 || r.Config.GoogleDriveTPSLimitBurst != 1 {
		t.Fatalf("zero config was not normalized: %+v", r.Config)
	}
}

func hasArgs(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		if reflect.DeepEqual(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

func containsArgPair(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
