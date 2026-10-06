package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayeus/ayeusann/internal/release"
)

// publish signs a release of the given version into dir and returns the key's
// public half.
func publish(t *testing.T, dir, seed, version, minVersion string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "ayeusann-agent-linux-amd64"), []byte("agent "+version), 0o755); err != nil {
		t.Fatal(err)
	}
	key, err := release.PrivateKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	m, err := release.Build(dir, version, minVersion, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := release.Sign(dir, m, key); err != nil {
		t.Fatal(err)
	}
}

func TestReleasesOfferAndMinimumVersion(t *testing.T) {
	dir := t.TempDir()
	seed, pubB64, _ := release.GenerateKey()
	pub, _ := release.PublicKey(pubB64)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	updater := []string{capReplicaReport, capSelfUpdate}

	// No key configured: no updates, nobody is out of date, nothing to pin.
	var none *releases
	if none.offer("0.1.0", updater) != nil || none.outdated("0.1.0") || none.publicKey() != nil {
		t.Fatal("an installation without a release key offered or required something")
	}
	if newReleases(dir, nil, "", quiet) != nil {
		t.Fatal("releases were set up without a key")
	}

	// A key but nothing published yet.
	r := newReleases(dir, pub, "https://gpu.example/downloads", quiet)
	if r == nil || r.offer("0.1.0", updater) != nil || r.outdated("0.1.0") {
		t.Fatal("something was offered before anything was published")
	}
	if string(r.publicKey()) != string(pub) {
		t.Fatal("the key agents pin is not the configured one")
	}

	publish(t, dir, seed, "0.4.0", "0.3.5")
	if !r.reload() {
		t.Fatal("a newly published release was not noticed")
	}
	if r.reload() {
		t.Fatal("an unchanged release was reported as new")
	}

	msg := r.offer("0.3.0", updater)
	if msg == nil {
		t.Fatal("an agent that is behind was not offered the release")
	}
	u := msg.GetUpdate()
	if u.GetVersion() != "0.4.0" || u.GetDownloadUrl() != "https://gpu.example/downloads" || !u.GetMandatory() {
		t.Fatalf("offer = version %q url %q mandatory %v", u.GetVersion(), u.GetDownloadUrl(), u.GetMandatory())
	}
	// What is sent is exactly what was signed.
	if m, err := release.Verify(u.GetManifest(), string(u.GetSignature()), pub); err != nil || m.Version != "0.4.0" {
		t.Fatalf("the offer does not carry a manifest that verifies: %v", err)
	}
	if u := r.offer("0.3.9", updater).GetUpdate(); u == nil || u.GetMandatory() {
		t.Fatal("an agent above the minimum should be offered the update, not required to take it")
	}
	for _, v := range []string{"0.4.0", "0.4.1", "1.0.0"} {
		if r.offer(v, updater) != nil {
			t.Fatalf("an agent on %s was offered 0.4.0", v)
		}
	}
	if r.offer("0.3.0", []string{capReplicaReport}) != nil || r.offer("0.3.0", nil) != nil {
		t.Fatal("an agent that cannot update itself was sent an update")
	}

	for v, want := range map[string]bool{"0.3.0": true, "0.3.4": true, "0.3.5": false, "0.4.0": false, "dev": true, "": true} {
		if got := r.outdated(v); got != want {
			t.Errorf("outdated(%q) = %v, want %v", v, got, want)
		}
	}

	// The same version published again with a build that was missing: the
	// machines waiting for that build have to hear about it.
	if err := os.WriteFile(filepath.Join(dir, "ayeusann-agent-linux-arm64"), []byte("arm build"), 0o755); err != nil {
		t.Fatal(err)
	}
	publish(t, dir, seed, "0.4.0", "0.3.5")
	if !r.reload() {
		t.Fatal("the same version republished with another build was not announced")
	}

	// Someone with access to the platform replaces the manifest. It is
	// ignored, and the last release that did verify stays in force.
	body, _ := os.ReadFile(filepath.Join(dir, release.ManifestFile))
	forged := []byte(string(body[:len(body)-2]) + " }\n")
	if err := os.WriteFile(filepath.Join(dir, release.ManifestFile), forged, 0o644); err != nil {
		t.Fatal(err)
	}
	if r.reload() {
		t.Fatal("an unsigned change to the manifest was taken as a new release")
	}
	if u := r.offer("0.3.0", updater).GetUpdate(); u == nil || string(u.GetManifest()) != string(body) {
		t.Fatal("after a forged manifest, agents are no longer offered the last good release")
	}
	// A release signed with another key is no better.
	otherSeed, _, _ := release.GenerateKey()
	publish(t, dir, otherSeed, "9.9.9", "9.9.9")
	if r.reload() || r.outdated("0.4.0") {
		t.Fatal("a release signed by a different key was accepted")
	}
	// No minimum named (the tool writes 0.0.0): nobody is out of date, not
	// even an agent whose version cannot be read.
	publish(t, dir, seed, "0.4.5", "0.0.0")
	if !r.reload() {
		t.Fatal("reload")
	}
	for _, v := range []string{"0.1.0", "dev", ""} {
		if r.outdated(v) {
			t.Errorf("with no minimum, an agent on %q was marked out of date", v)
		}
	}
	if u := r.offer("0.1.0", updater).GetUpdate(); u == nil || u.GetMandatory() {
		t.Fatal("with no minimum, the update should be offered and not required")
	}
	// The real publisher moves on.
	publish(t, dir, seed, "0.5.0", "0.4.0")
	if !r.reload() || r.offer("0.4.0", updater).GetUpdate().GetVersion() != "0.5.0" || !r.outdated("0.3.9") {
		t.Fatal("the next properly signed release was not picked up")
	}
}
