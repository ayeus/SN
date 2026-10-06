// Package release describes a published set of host-agent binaries and proves
// who published it.
//
// A release is a manifest (release.json) listing one binary per platform with
// its size and SHA-256, and a detached Ed25519 signature over the manifest's
// exact bytes (release.json.sig). The key that signs releases is separate from
// every key a running service holds: its private half lives with the person
// who publishes, never on the platform. Agents pin the public half when they
// enrol, so a platform that is broken into can withhold an update but cannot
// make a host run a binary its operator did not sign.
package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// ManifestFile and SignatureFile sit beside the binaries they describe.
	ManifestFile  = "release.json"
	SignatureFile = "release.json.sig"
)

// Artifact is one platform's binary.
type Artifact struct {
	OS     string `json:"os"`   // linux, darwin, windows
	Arch   string `json:"arch"` // amd64, arm64
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"` // lowercase hex
}

// Manifest is the signed description of a release.
type Manifest struct {
	Version string `json:"version"`
	// MinVersion is the oldest agent the platform still gives work to. Older
	// agents stay connected, so that they can update, and are given nothing
	// to run until they have.
	MinVersion  string     `json:"min_version"`
	PublishedAt time.Time  `json:"published_at"`
	Artifacts   []Artifact `json:"artifacts"`
}

// Find returns the artifact for a platform.
func (m *Manifest) Find(os, arch string) (Artifact, bool) {
	for _, a := range m.Artifacts {
		if a.OS == os && a.Arch == arch {
			return a, true
		}
	}
	return Artifact{}, false
}

// GenerateKey returns a new signing seed and its public key, both base64.
func GenerateKey() (seedB64, publicB64 string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv.Seed()), base64.StdEncoding.EncodeToString(pub), nil
}

// PrivateKey decodes a base64 seed.
func PrivateKey(seedB64 string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(seedB64))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("release: the signing key must be a base64 32-byte seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// PublicOf returns a key's public half, base64.
func PublicOf(key ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
}

// PublicKey decodes a base64 public key.
func PublicKey(publicB64 string) (ed25519.PublicKey, error) {
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(publicB64))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("release: the public key must be base64 of 32 bytes")
	}
	return ed25519.PublicKey(pub), nil
}

// artifactName matches the names the build targets give the binaries:
// ayeusann-agent-<os>-<arch>[.exe].
func artifactName(name string) (os, arch string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSuffix(name, ".exe"), "ayeusann-agent-")
	if !found {
		return "", "", false
	}
	os, arch, found = strings.Cut(rest, "-")
	if !found || os == "" || arch == "" || strings.Contains(arch, ".") {
		return "", "", false
	}
	return os, arch, true
}

// Build describes every agent binary found in dir.
func Build(dir, version, minVersion string, now time.Time) (*Manifest, error) {
	if _, err := ParseVersion(version); err != nil {
		return nil, err
	}
	if _, err := ParseVersion(minVersion); err != nil {
		return nil, fmt.Errorf("minimum version: %w", err)
	}
	if Compare(minVersion, version) > 0 {
		return nil, fmt.Errorf("release: the minimum version %s is newer than the release %s; no agent could ever satisfy it", minVersion, version)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Version: version, MinVersion: minVersion, PublishedAt: now.UTC().Truncate(time.Second)}
	for _, e := range entries {
		osName, arch, ok := artifactName(e.Name())
		if e.IsDir() || !ok {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return nil, err
		}
		m.Artifacts = append(m.Artifacts, Artifact{OS: osName, Arch: arch, File: e.Name(), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
	}
	if len(m.Artifacts) == 0 {
		return nil, fmt.Errorf("release: no agent binaries (ayeusann-agent-<os>-<arch>) in %s", dir)
	}
	sort.Slice(m.Artifacts, func(i, j int) bool { return m.Artifacts[i].File < m.Artifacts[j].File })
	return m, nil
}

// Sign writes the manifest and its signature into dir. The signature covers
// the bytes written, so the file must not be reformatted afterwards.
func Sign(dir string, m *Manifest, key ed25519.PrivateKey) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(key, body))
	// Two files cannot be replaced in one step, so a reader can catch the new
	// signature beside the old manifest for an instant. Load allows for that.
	if err := writeAtomic(filepath.Join(dir, SignatureFile), []byte(sig+"\n")); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, ManifestFile), body)
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Verify checks a manifest's signature and returns it parsed. body is the
// manifest exactly as published; sigB64 is the content of the signature file.
func Verify(body []byte, sigB64 string, pub ed25519.PublicKey) (*Manifest, error) {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil {
		return nil, errors.New("release: the signature is not valid base64")
	}
	if !ed25519.Verify(pub, body, sig) {
		return nil, errors.New("release: the manifest's signature does not match the release key")
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("release: the manifest is signed but unreadable: %w", err)
	}
	if _, err := ParseVersion(m.Version); err != nil {
		return nil, err
	}
	if m.MinVersion != "" {
		if _, err := ParseVersion(m.MinVersion); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

// Load reads and verifies the release published in dir.
func Load(dir string, pub ed25519.PublicKey) (*Manifest, []byte, string, error) {
	m, body, sig, err := load(dir, pub)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A release being published right now is half written for a moment.
		time.Sleep(300 * time.Millisecond)
		return load(dir, pub)
	}
	return m, body, sig, err
}

func load(dir string, pub ed25519.PublicKey) (*Manifest, []byte, string, error) {
	body, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, nil, "", err
	}
	sig, err := os.ReadFile(filepath.Join(dir, SignatureFile))
	if err != nil {
		return nil, nil, "", err
	}
	m, err := Verify(body, string(sig), pub)
	if err != nil {
		return nil, nil, "", err
	}
	return m, body, strings.TrimSpace(string(sig)), nil
}

// ParseVersion reads "MAJOR.MINOR.PATCH" (a leading "v" and anything after a
// "-" or "+" are ignored).
func ParseVersion(v string) ([3]int, error) {
	var out [3]int
	core := strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("release: %q is not a version (want MAJOR.MINOR.PATCH)", v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("release: %q is not a version (want MAJOR.MINOR.PATCH)", v)
		}
		out[i] = n
	}
	return out, nil
}

// Compare orders two versions: -1, 0 or 1. A version that cannot be read
// (an agent from before versions were real says "0.3.0"; a development build
// says "dev") sorts before every real one.
func Compare(a, b string) int {
	pa, errA := ParseVersion(a)
	pb, errB := ParseVersion(b)
	switch {
	case errA != nil && errB != nil:
		return 0
	case errA != nil:
		return -1
	case errB != nil:
		return 1
	}
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
