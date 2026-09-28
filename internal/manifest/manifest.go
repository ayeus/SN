// Package manifest signs job manifests (SRS FR-53, Architecture §6: "signed job
// manifests (Ed25519)"). The signature covers a canonical, language-neutral
// string rather than protobuf bytes, because protobuf serialisation is not
// canonical across the Go coordinator and the Rust agent.
package manifest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
)

// Canonical is the exact byte string that is signed. The agent reproduces it
// field-for-field; changing it is a protocol change.
func Canonical(m *agentv1.ManifestDispatch) []byte {
	fields := []string{
		"ayeusann-manifest-v1",
		m.GetJobId(),
		m.GetReplicaId(),
		m.GetDeploymentId(),
		m.GetModelId(),
		m.GetModelName(),
		m.GetRuntimeModel(),
		m.GetModelHash(),
		m.GetImageHash(),
		m.GetGpuUuid(),
		fmt.Sprintf("%d", m.GetIssuedAt()),
	}
	return []byte(strings.Join(fields, "\n"))
}

// Signer holds the coordinator's manifest signing key.
type Signer struct {
	priv ed25519.PrivateKey
	id   []byte
}

// NewSigner loads a key from a base64-encoded 32-byte seed. When seedB64 is empty
// and devSecret is set, a deterministic development key is derived from it so
// local agents keep verifying across coordinator restarts.
func NewSigner(seedB64, devSecret string) (*Signer, error) {
	var seed []byte
	switch {
	case seedB64 != "":
		b, err := base64.StdEncoding.DecodeString(seedB64)
		if err != nil {
			return nil, fmt.Errorf("manifest: signing key is not valid base64: %w", err)
		}
		if len(b) != ed25519.SeedSize {
			return nil, fmt.Errorf("manifest: signing key seed must be %d bytes, got %d", ed25519.SeedSize, len(b))
		}
		seed = b
	case devSecret != "":
		sum := sha256.Sum256([]byte("ayeusann-manifest-dev-key:" + devSecret))
		seed = sum[:]
	default:
		return nil, errors.New("manifest: MANIFEST_SIGNING_KEY is required")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	id := sha256.Sum256(pub)
	return &Signer{priv: priv, id: id[:8]}, nil
}

// PublicKey returns the verification key sent to agents at enrolment.
func (s *Signer) PublicKey() []byte { return s.priv.Public().(ed25519.PublicKey) }

// Sign fills the manifest's signature fields.
func (s *Signer) Sign(m *agentv1.ManifestDispatch) {
	m.Signature = ed25519.Sign(s.priv, Canonical(m))
	m.SigningKeyId = s.id
}

// Verify checks a manifest against a public key. The coordinator does not need
// it; it exists so tests pin the canonical form the agent implements.
func Verify(pub []byte, m *agentv1.ManifestDispatch) bool {
	return len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, Canonical(m), m.GetSignature())
}
