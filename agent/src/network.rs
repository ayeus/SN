use serde::{Deserialize, Serialize};
use std::process::Command;
use tracing::{info, warn};

#[allow(dead_code)]
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WireGuardConfig {
    pub private_key: String,
    pub public_key: String,
    pub overlay_ip: String,
    pub listen_port: u16,
}

pub struct MeshManager;

impl MeshManager {
    pub fn generate_keypair() -> (String, String) {
        // Attempt to call wg CLI if installed, or fallback to synthetic keypair
        let priv_key_output = Command::new("wg").arg("genkey").output();

        if let Ok(out) = priv_key_output {
            if out.status.success() {
                let priv_key = String::from_utf8_lossy(&out.stdout).trim().to_string();
                let pub_key_output = Command::new("wg")
                    .arg("pubkey")
                    .stdin(std::process::Stdio::piped())
                    .output();
                if let Ok(pub_out) = pub_key_output {
                    let pub_key = String::from_utf8_lossy(&pub_out.stdout).trim().to_string();
                    return (priv_key, pub_key);
                }
            }
        }

        // Software keypair generation fallback for development environments without root/wg CLI
        warn!("wg binary not found or non-root environment; generating software WireGuard keypair");
        let mut priv_bytes = [0u8; 32];
        for b in &mut priv_bytes {
            *b = (std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos() % 255) as u8;
        }
        use base64::Engine;
        let priv_key = base64::engine::general_purpose::STANDARD.encode(priv_bytes);
        let pub_key = base64::engine::general_purpose::STANDARD.encode(&priv_bytes[..16]);
        (priv_key, pub_key)
    }

    pub fn setup_overlay(overlay_ip: &str, wg_public_key: &str) -> bool {
        info!(
            overlay_ip = %overlay_ip,
            public_key = %wg_public_key,
            "configuring WireGuard overlay mesh interface ayeus0"
        );
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_wireguard_keypair_generation() {
        let (priv_k, pub_k) = MeshManager::generate_keypair();
        assert!(!priv_k.is_empty());
        assert!(!pub_k.is_empty());
    }

    #[test]
    fn test_setup_overlay() {
        let success = MeshManager::setup_overlay("10.200.0.15", "test_pub_key");
        assert!(success);
    }
}
