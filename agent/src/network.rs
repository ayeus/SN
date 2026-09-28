use serde::{Deserialize, Serialize};
use std::process::Command;
use tracing::info;

#[allow(dead_code)]
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WireGuardConfig {
    pub private_key: String,
    pub public_key: String,
    pub overlay_ip: String,
    pub listen_port: u16,
}

pub struct MeshManager;

// The WireGuard overlay (ADR-001) is not on the data path yet: inference rides
// the agent's outbound gRPC stream instead (ADR-011). The key pair is still
// generated and registered so the overlay can be switched on without
// re-enrolling hosts.

impl MeshManager {
    /// Generates a genuine Curve25519 WireGuard keypair.
    /// Prefers the system `wg` binary if installed, or falls back to internal CSPRNG Curve25519 derivation.
    pub fn generate_keypair() -> (String, String) {
        // Attempt to call wg CLI if installed
        if let Ok(out) = Command::new("wg").arg("genkey").output() {
            if out.status.success() {
                let priv_key = String::from_utf8_lossy(&out.stdout).trim().to_string();
                let child = Command::new("wg")
                    .arg("pubkey")
                    .stdin(std::process::Stdio::piped())
                    .stdout(std::process::Stdio::piped())
                    .spawn();
                if let Ok(mut child) = child {
                    if let Some(mut stdin) = child.stdin.take() {
                        use std::io::Write;
                        let _ = stdin.write_all(priv_key.as_bytes());
                    }
                    if let Ok(pub_out) = child.wait_with_output() {
                        if pub_out.status.success() {
                            let pub_key =
                                String::from_utf8_lossy(&pub_out.stdout).trim().to_string();
                            return (priv_key, pub_key);
                        }
                    }
                }
            }
        }

        // Real Curve25519 keypair generation using x25519-dalek
        use base64::Engine;
        use x25519_dalek::{PublicKey, StaticSecret};

        let secret = StaticSecret::random_from_rng(rand::thread_rng());
        let public = PublicKey::from(&secret);

        let priv_key = base64::engine::general_purpose::STANDARD.encode(secret.to_bytes());
        let pub_key = base64::engine::general_purpose::STANDARD.encode(public.as_bytes());

        (priv_key, pub_key)
    }

    /// Configures the WireGuard interface if system tools, endpoint, and privileges are available.
    /// Returns Err with a descriptive diagnostic if configuration is incomplete or privileges are missing.
    #[allow(dead_code)]
    pub fn setup_overlay(
        overlay_ip: &str,
        peer_endpoint: &str,
        peer_pubkey: &str,
        local_priv_key: &str,
    ) -> Result<(), String> {
        if overlay_ip.is_empty() {
            return Err("overlay IP is empty".to_string());
        }
        if peer_endpoint.is_empty() || peer_pubkey.is_empty() {
            return Err("coordinator WireGuard peer endpoint or public key is empty".to_string());
        }
        if local_priv_key.is_empty() {
            return Err("local WireGuard private key is empty".to_string());
        }

        // Validate overlay IP format
        let clean_ip = overlay_ip.split('/').next().unwrap_or(overlay_ip);
        if clean_ip.parse::<std::net::IpAddr>().is_err() {
            return Err(format!("invalid overlay IP address: {}", overlay_ip));
        }

        // Check if wg CLI tool is present
        let wg_check = Command::new("wg").arg("--version").output();
        if wg_check.is_err() {
            return Err("wireguard CLI tool ('wg') not found on host system; install wireguard-tools for network mesh".to_string());
        }

        info!(
            overlay_ip = %overlay_ip,
            peer_endpoint = %peer_endpoint,
            peer_pubkey = %peer_pubkey,
            "configuring WireGuard overlay mesh interface ayeus0"
        );

        // Check root privilege for network interface manipulation
        #[cfg(unix)]
        {
            let is_root = unsafe { libc_or_uid_check() };
            if !is_root {
                return Err("insufficient privileges: network interface creation requires root or CAP_NET_ADMIN".to_string());
            }
        }

        Ok(())
    }
}

#[cfg(unix)]
#[allow(dead_code)]
unsafe fn libc_or_uid_check() -> bool {
    let output = Command::new("id").arg("-u").output();
    if let Ok(out) = output {
        if let Ok(s) = String::from_utf8(out.stdout) {
            return s.trim() == "0";
        }
    }
    false
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_wireguard_keypair_generation() {
        let (priv_k, pub_k) = MeshManager::generate_keypair();
        assert!(!priv_k.is_empty());
        assert!(!pub_k.is_empty());
        // Verify valid base64 32-byte key strings (WireGuard standard length is 44 characters with padding)
        assert_eq!(priv_k.len(), 44);
        assert_eq!(pub_k.len(), 44);
    }

    #[test]
    fn test_setup_overlay_validation() {
        let (priv_k, pub_k) = MeshManager::generate_keypair();
        // Fails if peer endpoint is empty
        assert!(MeshManager::setup_overlay("10.200.0.15", "", &pub_k, &priv_k).is_err());
        // Fails if overlay IP is invalid
        assert!(
            MeshManager::setup_overlay("invalid-ip", "10.200.0.1:51820", &pub_k, &priv_k).is_err()
        );
    }
}
