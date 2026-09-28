//! Manifest signature verification (SRS FR-53: "verifies manifest signatures
//! before execution"). The canonical form must match internal/manifest in the
//! Go coordinator byte for byte.

use crate::proto::ManifestDispatch;
use anyhow::{bail, Result};
use ed25519_dalek::{Signature, Verifier, VerifyingKey};
use std::time::{SystemTime, UNIX_EPOCH};

/// A manifest older than this is refused, bounding replay of a captured one.
/// Hosts must keep NTP-synced clocks (SRS NFR-14).
const MAX_AGE_SECS: i64 = 15 * 60;

pub fn canonical(m: &ManifestDispatch) -> String {
    [
        "ayeusann-manifest-v1",
        &m.job_id,
        &m.replica_id,
        &m.deployment_id,
        &m.model_id,
        &m.model_name,
        &m.runtime_model,
        &m.model_hash,
        &m.image_hash,
        &m.gpu_uuid,
        &m.issued_at.to_string(),
    ]
    .join("\n")
}

/// Verifies a manifest against the coordinator key pinned at enrolment.
pub fn verify(public_key: &[u8], m: &ManifestDispatch) -> Result<()> {
    let key: [u8; 32] = match public_key.try_into() {
        Ok(k) => k,
        Err(_) => bail!("no valid manifest signing key is pinned; re-enrol this host"),
    };
    let vk = VerifyingKey::from_bytes(&key)?;
    let sig: [u8; 64] = match m.signature.as_slice().try_into() {
        Ok(s) => s,
        Err(_) => bail!("manifest is unsigned"),
    };
    vk.verify(canonical(m).as_bytes(), &Signature::from_bytes(&sig))
        .map_err(|_| anyhow::anyhow!("manifest signature does not verify"))?;

    let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;
    if (now - m.issued_at).abs() > MAX_AGE_SECS {
        bail!("manifest timestamp is outside the accepted window; check this machine's clock");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use ed25519_dalek::{Signer, SigningKey};

    fn manifest(issued_at: i64) -> ManifestDispatch {
        ManifestDispatch {
            job_id: "j".into(),
            replica_id: "r".into(),
            issued_at,
            ..Default::default()
        }
    }

    #[test]
    fn canonical_form_matches_the_coordinator() {
        // Same vector as internal/manifest TestCanonicalForm in Go.
        assert_eq!(
            canonical(&manifest(5)),
            "ayeusann-manifest-v1\nj\nr\n\n\n\n\n\n\n\n5"
        );
    }

    #[test]
    fn verifies_signed_and_rejects_tampered_or_stale() {
        let sk = SigningKey::from_bytes(&[7u8; 32]);
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64;
        let mut m = manifest(now);
        m.runtime_model = "qwen2.5:7b".into();
        m.signature = sk.sign(canonical(&m).as_bytes()).to_bytes().to_vec();
        let pk = sk.verifying_key().to_bytes();

        assert!(verify(&pk, &m).is_ok());

        let mut tampered = m.clone();
        tampered.runtime_model = "evil:latest".into();
        assert!(verify(&pk, &tampered).is_err());

        let mut stale = manifest(now - 3600);
        stale.signature = sk.sign(canonical(&stale).as_bytes()).to_bytes().to_vec();
        assert!(verify(&pk, &stale).is_err());

        assert!(verify(&[], &m).is_err());
    }
}
