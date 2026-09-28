//! Durable agent identity, persisted between runs.
//!
//! Registration tokens are single-use. After the first enrolment the agent
//! reconnects with the host credential stored here, and it pins the
//! coordinator's manifest-signing key so a later impostor cannot issue jobs.

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};

#[derive(Debug, Default, Clone, Serialize, Deserialize)]
pub struct State {
    pub host_id: Option<String>,
    pub host_credential: Option<String>,
    /// Base64 Ed25519 public key that signs manifests.
    pub manifest_public_key: Option<String>,
    pub coordinator_url: Option<String>,
    pub tier: Option<String>,
    /// Unix seconds of the last benchmark sent (PRD F-12: weekly re-runs).
    pub last_benchmark_at: Option<i64>,
}

pub fn path(data_dir: &Path) -> PathBuf {
    data_dir.join("agent-state.json")
}

pub fn load(data_dir: &Path) -> State {
    std::fs::read(path(data_dir))
        .ok()
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default()
}

/// Writes the state with owner-only permissions: it holds a credential.
pub fn save(data_dir: &Path, state: &State) -> Result<()> {
    std::fs::create_dir_all(data_dir)
        .with_context(|| format!("creating {}", data_dir.display()))?;
    let p = path(data_dir);
    let tmp = p.with_extension("json.tmp");
    std::fs::write(&tmp, serde_json::to_vec_pretty(state)?)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&tmp, std::fs::Permissions::from_mode(0o600))?;
    }
    std::fs::rename(&tmp, &p)?;
    Ok(())
}

pub fn clear(data_dir: &Path) {
    let _ = std::fs::remove_file(path(data_dir));
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn round_trips_with_private_permissions() {
        let dir = std::env::temp_dir().join(format!("ayeusann-state-test-{}", std::process::id()));
        let s = State {
            host_id: Some("h".into()),
            host_credential: Some("hc_x".into()),
            ..Default::default()
        };
        save(&dir, &s).unwrap();
        let back = load(&dir);
        assert_eq!(back.host_credential.as_deref(), Some("hc_x"));
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = std::fs::metadata(path(&dir)).unwrap().permissions().mode() & 0o777;
            assert_eq!(mode, 0o600);
        }
        clear(&dir);
        assert!(load(&dir).host_credential.is_none());
        let _ = std::fs::remove_dir_all(&dir);
    }
}
