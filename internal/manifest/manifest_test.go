package manifest

import (
	"testing"

	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
)

func TestSignAndVerify(t *testing.T) {
	s, err := NewSigner("", "dev-secret-for-tests-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	m := &agentv1.ManifestDispatch{
		JobId: "j", ReplicaId: "r", DeploymentId: "d", ModelId: "m", ModelName: "qwen2.5-7b-instruct",
		RuntimeModel: "qwen2.5:7b", GpuUuid: "GPU-1", IssuedAt: 1790000000,
	}
	s.Sign(m)
	if !Verify(s.PublicKey(), m) {
		t.Fatal("signature did not verify")
	}

	m.RuntimeModel = "evil:latest"
	if Verify(s.PublicKey(), m) {
		t.Fatal("tampered manifest verified")
	}
}

func TestDevKeyIsDeterministic(t *testing.T) {
	a, _ := NewSigner("", "same")
	b, _ := NewSigner("", "same")
	if string(a.PublicKey()) != string(b.PublicKey()) {
		t.Fatal("dev key must be stable across restarts")
	}
	if _, err := NewSigner("", ""); err == nil {
		t.Fatal("missing key must be an error")
	}
}

func TestCanonicalForm(t *testing.T) {
	// The Rust agent reproduces this exact string; keep them in lockstep.
	m := &agentv1.ManifestDispatch{JobId: "j", ReplicaId: "r", IssuedAt: 5}
	want := "ayeusann-manifest-v1\nj\nr\n\n\n\n\n\n\n\n5"
	if got := string(Canonical(m)); got != want {
		t.Fatalf("canonical form changed:\n%q\n%q", got, want)
	}
}
