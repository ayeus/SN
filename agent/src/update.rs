//! Self-update from a signed release.
//!
//! The platform tells the agent a newer version exists and hands over the
//! release manifest with its signature. Nothing in that message is trusted as
//! it stands: the manifest must verify against the release key this agent
//! pinned when it enrolled, and the binary must match the size and SHA-256 the
//! manifest gives for this platform. A platform that has been broken into can
//! withhold an update; it cannot make this machine run something its operator
//! did not sign.
//!
//! The new binary is test-run before it replaces the old one, the old one is
//! kept beside it, and if the new one has not connected within ten minutes it
//! is swapped back (see `Pending` and the checks in main.rs).
//!
//! The signature scheme and manifest format are internal/release in the Go
//! code; internal/release/testdata/vector.json is checked by both.

use anyhow::{anyhow, bail, Context, Result};
use base64::Engine;
use ed25519_dalek::{Signature, Verifier, VerifyingKey};
use futures_util::StreamExt;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::cmp::Ordering;
use std::path::{Path, PathBuf};
use std::time::Duration;

/// The version this binary was built as.
pub const VERSION: &str = env!("SN_AGENT_VERSION");

/// How long a new version has to connect before the old one is put back,
/// provided it has really tried: time alone proves nothing when the machine
/// was asleep or the platform was away.
pub const CONFIRM_WITHIN_SECS: i64 = 600;
/// Failed attempts to connect, on top of the time, before giving up on it.
pub const CONFIRM_FAILURES: u32 = 3;
/// Starts without ever connecting that mean the new version is crashing.
pub const CONFIRM_STARTS: u32 = 6;
/// How long a version that was put back is left alone before it is tried again.
pub const SKIP_FOR_SECS: i64 = 24 * 3600;

#[derive(Debug, Clone, Deserialize)]
pub struct Artifact {
    pub os: String,
    pub arch: String,
    pub file: String,
    pub size: u64,
    pub sha256: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Manifest {
    pub version: String,
    #[serde(default)]
    pub artifacts: Vec<Artifact>,
}

/// An update that has been installed and not yet proven to work.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Pending {
    pub from: String,
    pub to: String,
    /// Unix seconds when the binaries were swapped.
    pub at: i64,
    /// Times the new version has been started without connecting yet.
    #[serde(default)]
    pub starts: u32,
    /// Times the new version tried to connect and could not.
    #[serde(default)]
    pub failures: u32,
}

/// What the platform offered, as received.
#[derive(Debug, Clone)]
pub struct Offer {
    pub manifest: Vec<u8>,
    pub signature: Vec<u8>,
    /// Base address of the binaries; empty means under the address in use.
    pub download_url: String,
    /// The address this agent is connected to.
    pub connected_via: String,
}

fn parse(v: &str) -> Option<[u64; 3]> {
    let core = v.trim().trim_start_matches('v');
    let core = core.split(['-', '+']).next().unwrap_or(core);
    let mut parts = core.split('.');
    let out = [
        parts.next()?.parse().ok()?,
        parts.next()?.parse().ok()?,
        parts.next()?.parse().ok()?,
    ];
    parts.next().is_none().then_some(out)
}

/// Orders two versions the way the platform does: numerically, and a version
/// that cannot be read sorts before every real one.
pub fn compare(a: &str, b: &str) -> Ordering {
    match (parse(a), parse(b)) {
        (Some(x), Some(y)) => x.cmp(&y),
        (None, None) => Ordering::Equal,
        (None, Some(_)) => Ordering::Less,
        (Some(_), None) => Ordering::Greater,
    }
}

/// Checks a manifest's signature against the pinned release key and returns
/// it parsed. `signature` is base64, as published in release.json.sig.
pub fn verify(public_key: &[u8], manifest: &[u8], signature: &[u8]) -> Result<Manifest> {
    let key: [u8; 32] = public_key
        .try_into()
        .map_err(|_| anyhow!("no valid release key is pinned"))?;
    let key = VerifyingKey::from_bytes(&key).context("the pinned release key is not a key")?;
    let sig = base64::engine::general_purpose::STANDARD
        .decode(String::from_utf8_lossy(signature).trim())
        .map_err(|_| anyhow!("the release signature is not valid base64"))?;
    let sig: [u8; 64] = sig
        .as_slice()
        .try_into()
        .map_err(|_| anyhow!("the release signature has the wrong length"))?;
    key.verify(manifest, &Signature::from_bytes(&sig))
        .map_err(|_| anyhow!("the release is not signed by the key this machine trusts"))?;
    let m: Manifest = serde_json::from_slice(manifest)
        .context("the release manifest is signed but unreadable")?;
    if parse(&m.version).is_none() {
        bail!("the release manifest has no usable version");
    }
    Ok(m)
}

/// This platform, in the names the release uses.
pub fn platform() -> (&'static str, &'static str) {
    let os = match std::env::consts::OS {
        "macos" => "darwin",
        other => other,
    };
    let arch = match std::env::consts::ARCH {
        "x86_64" => "amd64",
        "aarch64" => "arm64",
        other => other,
    };
    (os, arch)
}

fn sibling(exe: &Path, suffix: &str) -> PathBuf {
    let name = exe
        .file_name()
        .map(|n| n.to_string_lossy().to_string())
        .unwrap_or_else(|| "ayeusann-agent".into());
    exe.with_file_name(format!("{name}.{suffix}"))
}

/// Where the binary this one replaced is kept.
pub fn previous_path(exe: &Path) -> PathBuf {
    sibling(exe, "previous")
}

/// Downloads this platform's binary from the release, checks it against the
/// manifest, and proves it starts. Returns the path of the checked file, which
/// sits beside `exe` ready to be swapped in.
pub async fn fetch(offer: &Offer, manifest: &Manifest, exe: &Path) -> Result<PathBuf> {
    let (os, arch) = platform();
    let artifact = manifest
        .artifacts
        .iter()
        .find(|a| a.os == os && a.arch == arch)
        .ok_or_else(|| anyhow!("release {} has no build for {os}/{arch}", manifest.version))?;
    // The name comes from a signed manifest, but it is still only joined to a
    // URL as a single path segment.
    if artifact.file.is_empty()
        || artifact.file.contains(['/', '\\'])
        || artifact.file.contains("..")
    {
        bail!("the release names a file that is not a plain file name");
    }
    let base = if offer.download_url.trim().is_empty() {
        format!("{}/downloads", offer.connected_via.trim_end_matches('/'))
    } else {
        offer.download_url.trim_end_matches('/').to_string()
    };
    let url = format!("{base}/{}", artifact.file);

    // Named for this process: two agents sharing one binary (development
    // instances) must not write over each other's download.
    let staged = sibling(exe, &format!("new-{}", std::process::id()));
    let _ = std::fs::remove_file(&staged);
    let http = reqwest::Client::builder()
        .connect_timeout(Duration::from_secs(15))
        .timeout(Duration::from_secs(900))
        .build()?;
    let resp = http
        .get(&url)
        .send()
        .await
        .with_context(|| format!("downloading {url}"))?;
    if !resp.status().is_success() {
        bail!("downloading {url}: {}", resp.status());
    }
    let mut out = tokio::fs::File::create(&staged)
        .await
        .with_context(|| format!("writing {}", staged.display()))?;
    let mut hash = Sha256::new();
    let mut size: u64 = 0;
    let mut body = resp.bytes_stream();
    while let Some(chunk) = body.next().await {
        let chunk = chunk.context("download interrupted")?;
        size += chunk.len() as u64;
        if size > artifact.size {
            drop(out);
            let _ = std::fs::remove_file(&staged);
            bail!(
                "the download is larger than the release says ({} bytes)",
                artifact.size
            );
        }
        hash.update(&chunk);
        tokio::io::AsyncWriteExt::write_all(&mut out, &chunk).await?;
    }
    tokio::io::AsyncWriteExt::flush(&mut out).await?;
    drop(out);

    let digest = hex::encode(hash.finalize());
    if size != artifact.size || !digest.eq_ignore_ascii_case(&artifact.sha256) {
        let _ = std::fs::remove_file(&staged);
        bail!(
            "the downloaded file is not the one that was signed (got {size} bytes, sha256 {digest}; the release says {} bytes, {})",
            artifact.size,
            artifact.sha256
        );
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&staged, std::fs::Permissions::from_mode(0o755))?;
    }

    // It must run on this machine and be the version it claims to be, before
    // it is allowed anywhere near the place the supervisor starts.
    if reported_version(&staged).await.as_deref() != Some(manifest.version.as_str()) {
        let _ = std::fs::remove_file(&staged);
        bail!(
            "the new version does not start on this machine, or is not version {}; keeping the current one",
            manifest.version
        );
    }
    Ok(staged)
}

/// Runs a binary with `--version` and returns the version it prints.
pub async fn reported_version(binary: &Path) -> Option<String> {
    let mut cmd = tokio::process::Command::new(binary);
    cmd.arg("--version").kill_on_drop(true);
    #[cfg(windows)]
    cmd.creation_flags(CREATE_NO_WINDOW);
    let out = tokio::time::timeout(Duration::from_secs(20), cmd.output())
        .await
        .ok()?
        .ok()?;
    if !out.status.success() {
        return None;
    }
    String::from_utf8_lossy(&out.stdout)
        .split_whitespace()
        .last()
        .map(str::to_string)
}

/// The child gets no console window of its own (the background agent has none
/// to share, and Windows would otherwise open one on the owner's desktop).
#[cfg(windows)]
const CREATE_NO_WINDOW: u32 = 0x0800_0000;
#[cfg(windows)]
const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;

/// Puts the checked binary in place of the running one, keeping the old one
/// beside it. Renames only: a running program's file can be renamed on every
/// platform, including Windows, where it cannot be overwritten.
pub fn swap(staged: &Path, exe: &Path) -> Result<()> {
    let previous = previous_path(exe);
    let _ = std::fs::remove_file(&previous);
    std::fs::rename(exe, &previous)
        .with_context(|| format!("moving the current version aside ({})", exe.display()))?;
    if let Err(e) = std::fs::rename(staged, exe) {
        // Put the old one back rather than leave nothing where the supervisor looks.
        let _ = std::fs::rename(&previous, exe);
        return Err(e).context("putting the new version in place");
    }
    Ok(())
}

/// Puts the previous binary back. Returns false when there is none to go back to.
pub fn rollback(exe: &Path) -> Result<bool> {
    let previous = previous_path(exe);
    if !previous.exists() {
        return Ok(false);
    }
    let failed = sibling(exe, "failed");
    let _ = std::fs::remove_file(&failed);
    std::fs::rename(exe, &failed).context("moving the failed version aside")?;
    if let Err(e) = std::fs::rename(&previous, exe) {
        let _ = std::fs::rename(&failed, exe);
        return Err(e).context("restoring the previous version");
    }
    Ok(true)
}

/// Starts the binary now at `exe` with this process's arguments, in place of
/// this process.
pub fn restart(exe: &Path) -> anyhow::Error {
    let args = restart_args(std::env::args_os().skip(1));
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        // exec only returns if it failed.
        anyhow!(std::process::Command::new(exe).args(args).exec())
    }
    #[cfg(not(unix))]
    {
        use std::os::windows::process::CommandExt;
        match std::process::Command::new(exe)
            .args(args)
            .creation_flags(CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP)
            .spawn()
        {
            Ok(_) => std::process::exit(0),
            Err(e) => anyhow!(e),
        }
    }
}

/// The arguments to start the next binary with: this process's own, minus the
/// ones that were for one occasion only. `--reset` would wipe the enrolment a
/// second time, and `--token` names a token that has already been used.
pub fn restart_args(args: impl Iterator<Item = std::ffi::OsString>) -> Vec<std::ffi::OsString> {
    let mut out = Vec::new();
    let mut skip_value = false;
    for a in args {
        if skip_value {
            skip_value = false;
            continue;
        }
        match a.to_str() {
            Some("--reset") => {}
            Some("--token") => skip_value = true,
            Some(s) if s.starts_with("--token=") => {}
            _ => out.push(a),
        }
    }
    out
}

pub fn now() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[derive(Deserialize)]
    struct Vector {
        public_key: String,
        manifest: String,
        signature: String,
    }

    fn vector() -> (Vec<u8>, Vector) {
        let v: Vector =
            serde_json::from_str(include_str!("../../internal/release/testdata/vector.json"))
                .unwrap();
        let key = base64::engine::general_purpose::STANDARD
            .decode(&v.public_key)
            .unwrap();
        (key, v)
    }

    /// The Go side signs and checks the same file (internal/release). If the
    /// two disagree about what a valid release is, updates are either
    /// impossible or unsafe.
    #[test]
    fn accepts_the_shared_vector_and_nothing_else() {
        let (key, v) = vector();
        let m = verify(&key, v.manifest.as_bytes(), v.signature.as_bytes()).unwrap();
        assert_eq!(m.version, "1.2.3");
        assert_eq!(m.artifacts.len(), 1);
        assert_eq!(m.artifacts[0].file, "ayeusann-agent-linux-amd64");
        assert_eq!(m.artifacts[0].size, 11);

        // The signature file ends with a newline; that is still the signature.
        let with_newline = format!("{}\n", v.signature);
        assert!(verify(&key, v.manifest.as_bytes(), with_newline.as_bytes()).is_ok());

        let tampered = v.manifest.replace("\"1.2.3\"", "\"1.2.4\"");
        assert!(verify(&key, tampered.as_bytes(), v.signature.as_bytes()).is_err());
        let longer = format!("{} ", v.manifest);
        assert!(verify(&key, longer.as_bytes(), v.signature.as_bytes()).is_err());

        let mut other = key.clone();
        other[0] ^= 1;
        assert!(verify(&other, v.manifest.as_bytes(), v.signature.as_bytes()).is_err());
        assert!(verify(&[], v.manifest.as_bytes(), v.signature.as_bytes()).is_err());
        assert!(verify(&key, v.manifest.as_bytes(), b"not base64!").is_err());
        assert!(verify(&key, v.manifest.as_bytes(), b"").is_err());
    }

    #[test]
    fn compares_versions_like_the_platform() {
        use Ordering::*;
        for (a, b, want) in [
            ("0.4.0", "0.4.0", Equal),
            ("0.4.0", "0.4.1", Less),
            ("0.10.0", "0.9.9", Greater),
            ("1.0.0", "0.99.99", Greater),
            ("v0.4.0", "0.4.0", Equal),
            ("0.4.0-rc1", "0.4.0", Equal),
            ("dev", "0.0.1", Less),
            ("", "0.4.0", Less),
            ("0.4.0", "dev", Greater),
            ("dev", "nonsense", Equal),
            ("0.4", "0.4.0", Less),
        ] {
            assert_eq!(compare(a, b), want, "compare({a:?}, {b:?})");
        }
        assert!(
            parse(VERSION).is_some(),
            "this build's version is {VERSION:?}"
        );
    }

    #[test]
    fn swaps_in_the_new_binary_and_can_put_the_old_one_back() {
        let dir = std::env::temp_dir().join(format!("sn-update-test-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let exe = dir.join("ayeusann-agent");
        let staged = sibling(&exe, "new-1");
        std::fs::write(&exe, "old").unwrap();
        std::fs::write(&staged, "new").unwrap();

        swap(&staged, &exe).unwrap();
        assert_eq!(std::fs::read_to_string(&exe).unwrap(), "new");
        assert_eq!(std::fs::read_to_string(previous_path(&exe)).unwrap(), "old");
        assert!(!staged.exists());

        // The new one never connects: back to the old one, exactly once.
        assert!(rollback(&exe).unwrap());
        assert_eq!(std::fs::read_to_string(&exe).unwrap(), "old");
        assert_eq!(
            std::fs::read_to_string(sibling(&exe, "failed")).unwrap(),
            "new"
        );
        assert!(!rollback(&exe).unwrap(), "nothing left to go back to");
        assert_eq!(std::fs::read_to_string(&exe).unwrap(), "old");

        // A swap whose new file is missing leaves the current one where it was.
        assert!(swap(&staged, &exe).is_err());
        assert_eq!(std::fs::read_to_string(&exe).unwrap(), "old");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_restart_drops_the_arguments_that_were_for_once() {
        let given = [
            "--reset",
            "--token",
            "eyJ.once",
            "--coordinator",
            "http://h:8080",
            "--token=eyJ.again",
            "--region",
            "IN-SOUTH",
            "--background",
        ];
        let kept: Vec<String> = restart_args(given.iter().map(std::ffi::OsString::from))
            .into_iter()
            .map(|a| a.to_string_lossy().to_string())
            .collect();
        assert_eq!(
            kept,
            [
                "--coordinator",
                "http://h:8080",
                "--region",
                "IN-SOUTH",
                "--background"
            ]
        );
    }

    #[test]
    fn names_this_platform_the_way_releases_do() {
        let (os, arch) = platform();
        assert!(["linux", "darwin", "windows"].contains(&os), "{os}");
        assert!(["amd64", "arm64"].contains(&arch), "{arch}");
    }
}
