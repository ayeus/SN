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
    /// The address this machine was told to use (`--coordinator`).
    pub coordinator_url: Option<String>,
    /// Other addresses the platform says it can be reached at, best first.
    #[serde(default)]
    pub coordinator_urls: Vec<String>,
    /// The address the last successful connection used; tried first next time.
    pub last_good_url: Option<String>,
    pub tier: Option<String>,
    /// Unix seconds of the last benchmark sent (PRD F-12: weekly re-runs).
    pub last_benchmark_at: Option<i64>,

    // Settings from the last successful enrolment or `service install`, so the
    // agent can be started again with no arguments.
    pub region: Option<String>,
    pub runtime: Option<String>,
    pub runtime_url: Option<String>,
    pub fake_gpu: Option<bool>,
    /// A registration token waiting to be used by a background start. Cleared
    /// as soon as the coordinator accepts it.
    pub pending_token: Option<String>,
    /// Why the coordinator last refused this machine, if it did.
    pub last_error: Option<String>,
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

/// The addresses to try, in order: the one that worked last, the one this
/// machine was configured with, then the rest of the platform's list.
pub fn coordinator_candidates(
    last_good: Option<&str>,
    configured: &str,
    pushed: &[String],
) -> Vec<String> {
    let mut out: Vec<String> = Vec::new();
    let all = last_good
        .into_iter()
        .chain(std::iter::once(configured))
        .chain(pushed.iter().map(String::as_str));
    for url in all {
        let url = url.trim().trim_end_matches('/');
        if !url.is_empty() && !out.iter().any(|u| u == url) {
            out.push(url.to_string());
        }
    }
    out
}

/// Whether a URL points at this machine. An agent on the platform's own
/// machine keeps using that address and ignores the list meant for others.
pub fn is_loopback(url: &str) -> bool {
    let rest = url.split("://").nth(1).unwrap_or(url);
    let authority = rest.split('/').next().unwrap_or(rest);
    let host = if let Some(v6) = authority.strip_prefix('[') {
        v6.split(']').next().unwrap_or("")
    } else {
        authority.rsplit_once(':').map_or(authority, |(h, _)| h)
    };
    host.eq_ignore_ascii_case("localhost")
        || host
            .parse::<std::net::IpAddr>()
            .map(|ip| ip.is_loopback())
            .unwrap_or(false)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn candidates_are_ordered_and_deduplicated() {
        let pushed = vec![
            "https://gpu.example.com/".to_string(),
            "http://192.168.1.4:8080".to_string(),
        ];
        assert_eq!(
            coordinator_candidates(
                Some("http://192.168.1.4:8080"),
                "http://10.0.0.2:8080",
                &pushed
            ),
            vec![
                "http://192.168.1.4:8080",
                "http://10.0.0.2:8080",
                "https://gpu.example.com"
            ]
        );
        assert_eq!(
            coordinator_candidates(None, "http://127.0.0.1:50051", &[]),
            vec!["http://127.0.0.1:50051"]
        );
    }

    #[test]
    fn recognises_loopback_addresses() {
        for url in [
            "http://127.0.0.1:50051",
            "http://localhost:8080",
            "https://LOCALHOST",
            "http://[::1]:8080/",
        ] {
            assert!(is_loopback(url), "{url}");
        }
        for url in [
            "http://192.168.1.4:8080",
            "https://gpu.example.com",
            "http://localhost.example.com:8080",
        ] {
            assert!(!is_loopback(url), "{url}");
        }
    }

    // An agent upgraded in place must keep its enrolment.
    #[test]
    fn reads_state_written_before_settings_were_remembered() {
        let old = br#"{"host_id":"h1","host_credential":"hc_x","manifest_public_key":"k","coordinator_url":"http://c:50051","tier":"t3","last_benchmark_at":1}"#;
        let st: State = serde_json::from_slice(old).unwrap();
        assert_eq!(st.host_credential.as_deref(), Some("hc_x"));
        assert_eq!(st.coordinator_url.as_deref(), Some("http://c:50051"));
        assert!(st.region.is_none() && st.pending_token.is_none() && st.last_error.is_none());
    }

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
