package platform

import (
	"strings"
	"testing"
)

const realSecret = "0123456789abcdef0123456789abcdef0123456789abcdef"

// The mode matrix: what each SN_ENV value allows. A private network is a real
// installation in every respect except the certificate.
func TestModeMatrix(t *testing.T) {
	cases := []struct {
		env       string
		mode      RunMode
		real      bool // real secrets and a fixed address required
		plaintext bool // may serve without TLS and without a proxy
		fakeGPU   bool // simulated GPUs may join
	}{
		{"dev", ModeDev, false, true, true},
		{"development", ModeDev, false, true, true},
		{"test", ModeTest, false, true, true},
		{"private", ModePrivate, true, true, false},
		{" Private ", ModePrivate, true, true, false},
		{"production", ModeProduction, true, false, false},
		{"", ModeProduction, true, false, false},
		{"prodution", ModeProduction, true, false, false}, // a typo fails closed
	}
	for _, tc := range cases {
		t.Setenv("SN_ENV", tc.env)
		t.Setenv("SN_TLS_TERMINATED_BY_PROXY", "")
		t.Setenv("ALLOW_FAKE_GPU", "")
		if got := Mode(); got != tc.mode {
			t.Errorf("SN_ENV=%q: Mode = %s, want %s", tc.env, got, tc.mode)
		}
		if got := IsProduction(); got != tc.real {
			t.Errorf("SN_ENV=%q: IsProduction = %v, want %v", tc.env, got, tc.real)
		}
		if got := AllowsPlaintext(); got != tc.plaintext {
			t.Errorf("SN_ENV=%q: AllowsPlaintext = %v, want %v", tc.env, got, tc.plaintext)
		}
		if got := AllowsFakeGPU(); got != tc.fakeGPU {
			t.Errorf("SN_ENV=%q: AllowsFakeGPU = %v, want %v", tc.env, got, tc.fakeGPU)
		}
	}
}

func TestProductionServesPlaintextOnlyBehindAProxy(t *testing.T) {
	t.Setenv("SN_ENV", "production")
	t.Setenv("SN_TLS_TERMINATED_BY_PROXY", "true")
	if !AllowsPlaintext() {
		t.Fatal("production behind a TLS-terminating proxy must be allowed to speak plain HTTP")
	}
}

func TestFakeGPUsNeedAnExplicitSwitchOnARealInstallation(t *testing.T) {
	for _, env := range []string{"private", "production"} {
		t.Setenv("SN_ENV", env)
		t.Setenv("ALLOW_FAKE_GPU", "true")
		if !AllowsFakeGPU() {
			t.Errorf("SN_ENV=%s with ALLOW_FAKE_GPU=true should admit simulated GPUs (end-to-end tests rely on it)", env)
		}
	}
}

// A private network must not start on the secrets published in this repository.
func TestPrivateModeRefusesDevelopmentSecrets(t *testing.T) {
	t.Setenv("SN_ENV", "private")

	t.Setenv("JWT_SECRET", "")
	if _, err := JWTSecret(); err == nil {
		t.Error("private mode started without JWT_SECRET")
	}
	t.Setenv("JWT_SECRET", DevJWTSecret)
	if _, err := JWTSecret(); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("private mode accepted the development sign-in secret: %v", err)
	}
	t.Setenv("INTERNAL_SERVICE_SECRET", DevInternalSecret)
	if _, err := InternalSecret(); err == nil {
		t.Error("private mode accepted the development service secret")
	}
	t.Setenv("JWT_SECRET", "too-short")
	if _, err := JWTSecret(); err == nil {
		t.Error("private mode accepted a short secret")
	}
	t.Setenv("JWT_SECRET", realSecret)
	if v, err := JWTSecret(); err != nil || v != realSecret {
		t.Errorf("private mode refused a real secret: %v", err)
	}
	t.Setenv("DATABASE_URL", "")
	if _, err := DatabaseURL(); err == nil {
		t.Error("private mode fell back to the development database")
	}
}

func TestRequirePublicURL(t *testing.T) {
	cases := []struct {
		env, url string
		ok       bool
	}{
		{"dev", "", true}, // development follows the request
		{"test", "", true},
		{"private", "", false},
		{"private", "http://192.168.1.4:8090", true},
		{"private", "http://100.101.102.103:8090", true},
		{"private", "https://gpu.example.net", true},
		{"private", "192.168.1.4:8090", false}, // no scheme
		{"private", "ftp://192.168.1.4", false},
		{"production", "", false},
		{"production", "http://app.example.com", false}, // a public installation needs TLS
		{"production", "https://app.example.com", true},
	}
	for _, tc := range cases {
		t.Setenv("SN_ENV", tc.env)
		t.Setenv("PUBLIC_URL", tc.url)
		err := RequirePublicURL()
		if (err == nil) != tc.ok {
			t.Errorf("SN_ENV=%s PUBLIC_URL=%q: err = %v, want ok=%v", tc.env, tc.url, err, tc.ok)
		}
	}
}
