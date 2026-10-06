package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type vector struct {
	Seed      string `json:"seed"`
	PublicKey string `json:"public_key"`
	Manifest  string `json:"manifest"`
	Signature string `json:"signature"`
}

func loadVector(t *testing.T) vector {
	t.Helper()
	b, err := os.ReadFile("testdata/vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vector
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The agent (Rust) checks the same file. If the two ever disagree about what
// a valid release is, an update either cannot be installed or should not be.
func TestSharedVector(t *testing.T) {
	v := loadVector(t)
	pub, err := PublicKey(v.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Verify([]byte(v.Manifest), v.Signature, pub)
	if err != nil {
		t.Fatalf("the shared vector does not verify: %v", err)
	}
	if m.Version != "1.2.3" || m.MinVersion != "1.0.0" || len(m.Artifacts) != 1 {
		t.Fatalf("manifest = %+v", m)
	}
	a, ok := m.Find("linux", "amd64")
	if !ok || a.Size != 11 || a.File != "ayeusann-agent-linux-amd64" {
		t.Fatalf("artifact = %+v", a)
	}
	if _, ok := m.Find("windows", "amd64"); ok {
		t.Fatal("found a platform that is not in the manifest")
	}

	// One changed byte anywhere, and it is no longer the signed release.
	tampered := strings.Replace(v.Manifest, `"1.2.3"`, `"1.2.4"`, 1)
	if _, err := Verify([]byte(tampered), v.Signature, pub); err == nil {
		t.Fatal("a manifest with a changed version verified")
	}
	if _, err := Verify([]byte(v.Manifest+" "), v.Signature, pub); err == nil {
		t.Fatal("a manifest with trailing bytes verified")
	}
	// Signed by another key.
	_, otherPub, _ := GenerateKey()
	other, _ := PublicKey(otherPub)
	if _, err := Verify([]byte(v.Manifest), v.Signature, other); err == nil {
		t.Fatal("a release verified against a key that did not sign it")
	}
	if _, err := Verify([]byte(v.Manifest), "not base64!", pub); err == nil {
		t.Fatal("a garbage signature verified")
	}
	// The key in the vector produces the signature in the vector.
	key, err := PrivateKey(v.Seed)
	if err != nil || PublicOf(key) != v.PublicKey {
		t.Fatalf("the vector's seed does not match its public key: %v", err)
	}
}

func TestBuildSignLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("ayeusann-agent-linux-amd64", "hello world")
	write("ayeusann-agent-windows-amd64.exe", "MZ")
	write("ayeusann-agent-darwin-arm64", "mach-o")
	write("notes.txt", "not a binary")
	write("release.json", "left over from before")

	seed, pubB64, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := PrivateKey(seed)
	pub, _ := PublicKey(pubB64)

	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m, err := Build(dir, "0.4.0", "0.3.0", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Artifacts) != 3 {
		t.Fatalf("%d artifacts, want the three binaries and nothing else: %+v", len(m.Artifacts), m.Artifacts)
	}
	linux, _ := m.Find("linux", "amd64")
	if linux.Size != 11 || linux.SHA256 != "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9" {
		t.Fatalf("linux artifact = %+v", linux)
	}
	if win, ok := m.Find("windows", "amd64"); !ok || win.File != "ayeusann-agent-windows-amd64.exe" {
		t.Fatalf("windows artifact = %+v", win)
	}
	if err := Sign(dir, m, key); err != nil {
		t.Fatal(err)
	}

	got, body, sig, err := Load(dir, pub)
	if err != nil {
		t.Fatalf("a freshly signed release does not load: %v", err)
	}
	if got.Version != "0.4.0" || got.MinVersion != "0.3.0" || !got.PublishedAt.Equal(now) {
		t.Fatalf("loaded = %+v", got)
	}
	if _, err := Verify(body, sig, pub); err != nil {
		t.Fatalf("what Load returned does not verify: %v", err)
	}

	// Someone edits the manifest on the platform to point at another binary.
	path := filepath.Join(dir, ManifestFile)
	edited := strings.Replace(string(body), linux.SHA256, strings.Repeat("0", 64), 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Load(dir, pub); err == nil {
		t.Fatal("an edited manifest loaded")
	}
}

func TestBuildRefusesNonsense(t *testing.T) {
	dir := t.TempDir()
	if _, err := Build(dir, "0.4.0", "0.0.0", time.Now()); err == nil {
		t.Error("a release with no binaries was built")
	}
	_ = os.WriteFile(filepath.Join(dir, "ayeusann-agent-linux-amd64"), []byte("x"), 0o755)
	for _, c := range []struct{ version, min string }{
		{"latest", "0.0.0"},
		{"0.4", "0.0.0"},
		{"0.4.0", "soon"},
		{"0.4.0", "0.5.0"}, // nobody could ever satisfy it, including this release
	} {
		if _, err := Build(dir, c.version, c.min, time.Now()); err == nil {
			t.Errorf("Build(%q, min %q) succeeded", c.version, c.min)
		}
	}
}

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.4.0", "0.4.0", 0},
		{"0.4.0", "0.4.1", -1},
		{"0.10.0", "0.9.9", 1}, // numbers, not strings
		{"1.0.0", "0.99.99", 1},
		{"v0.4.0", "0.4.0", 0},
		{"0.4.0-rc1", "0.4.0", 0},
		{"dev", "0.0.1", -1}, // an unreadable version is older than any real one
		{"", "0.4.0", -1},
		{"0.4.0", "dev", 1},
		{"dev", "nonsense", 0},
	} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
