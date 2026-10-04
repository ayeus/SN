mod benchmark;
mod gpu;
mod inference;
mod manifest;
mod network;
mod runtime;
mod session;
mod state;
mod telemetry;

// Generated code: tonic returns `Status` by value, which newer clippy flags.
#[allow(clippy::result_large_err)]
pub mod proto {
    tonic::include_proto!("ayeusann.agent.v1");
}

use anyhow::{bail, Result};
use benchmark::BenchmarkSuite;
use clap::Parser;
use gpu::GpuDetector;
use network::MeshManager;
use session::{HostFacts, Outcome, Session, Shared};
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;
use tracing::{error, info, warn};
use tracing_subscriber::{fmt, EnvFilter};

/// NVIDIA driver floor (PRD F-11: "NVIDIA R535+ enforced").
const MIN_NVIDIA_DRIVER: u32 = 535;

/// AyeusANN host agent: connects this machine's GPU to the network.
#[derive(Parser, Debug)]
#[command(name = "ayeusann-agent", version, about)]
struct Args {
    /// One-time registration token from the host console. Only needed for the
    /// first run; afterwards the agent reconnects with its stored credential.
    #[arg(long, env = "SN_REGISTRATION_TOKEN")]
    token: Option<String>,

    /// Coordinator gRPC endpoint.
    #[arg(
        long = "coordinator",
        visible_alias = "coordinator-url",
        env = "SN_COORDINATOR_URL",
        default_value = "http://127.0.0.1:50051"
    )]
    coordinator: String,

    /// Region this machine is in (e.g. IN-SOUTH).
    #[arg(long, env = "SN_REGION", default_value = "IN-SOUTH")]
    region: String,

    /// Model runtime to drive: ollama or vllm.
    #[arg(long, env = "SN_RUNTIME", default_value = "ollama")]
    runtime: String,

    /// Runtime base URL (default: the runtime's standard local port).
    #[arg(long, env = "SN_RUNTIME_URL")]
    runtime_url: Option<String>,

    /// Where the host credential is stored (default: ~/.ayeusann).
    #[arg(long, env = "SN_DATA_DIR")]
    data_dir: Option<PathBuf>,

    /// Heartbeat interval in seconds (SRS FR-40: 5 s).
    #[arg(long, env = "SN_HEARTBEAT_INTERVAL", default_value = "5")]
    heartbeat_interval: u64,

    /// Development only: report a simulated RTX 4090 instead of real hardware.
    #[arg(long, env = "SN_FAKE_GPU", default_value = "false")]
    fake_gpu: bool,

    /// Development only: run several agents on one machine as distinct hosts.
    #[arg(long, env = "SN_INSTANCE", hide = true)]
    instance: Option<String>,

    /// Forget the stored credential and enrol again with --token.
    #[arg(long)]
    reset: bool,
}

#[cfg(windows)]
fn command_output(cmd: &str, args: &[&str]) -> String {
    std::process::Command::new(cmd)
        .args(args)
        .output()
        .ok()
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
        .unwrap_or_default()
}

fn hostname() -> String {
    sysinfo::System::host_name()
        .filter(|h| !h.is_empty())
        .unwrap_or_else(|| "ayeusann-host".into())
}

/// The user's home directory: HOME on Unix, USERPROFILE on Windows.
fn home_dir() -> PathBuf {
    ["HOME", "USERPROFILE"]
        .iter()
        .find_map(|k| std::env::var_os(k).filter(|v| !v.is_empty()))
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("."))
}

fn os_name() -> String {
    match std::env::consts::OS {
        "macos" => "macOS".into(),
        "linux" => "Linux".into(),
        "windows" => "Windows".into(),
        o => o.into(),
    }
}

/// The operating system's own identifier for this installation, where it has
/// one that can be read without elevated rights.
#[cfg(windows)]
fn machine_id() -> Option<String> {
    // HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid, set at install time.
    command_output(
        "reg",
        &[
            "query",
            r"HKLM\SOFTWARE\Microsoft\Cryptography",
            "/v",
            "MachineGuid",
        ],
    )
    .lines()
    .find(|l| l.contains("MachineGuid"))
    .and_then(|l| l.split_whitespace().last())
    .map(str::to_string)
}

#[cfg(not(windows))]
fn machine_id() -> Option<String> {
    std::fs::read_to_string("/etc/machine-id")
        .ok()
        .map(|id| id.trim().to_string())
        .filter(|id| !id.is_empty())
}

/// A stable identity for this machine. Two agents on one machine are the same
/// host unless --instance says otherwise (development only).
fn fingerprint(gpus: &[gpu::GpuInfo], fake: bool, instance: Option<&str>) -> String {
    let base = if let (false, Some(g)) = (fake, gpus.first()) {
        format!("host:{}", g.fingerprint)
    } else if let Some(id) = machine_id() {
        format!("machine:{id}")
    } else {
        format!("hostname:{}", hostname())
    };
    let salted = match (fake, instance) {
        (_, Some(i)) => format!("{base}|instance:{i}|fake:{fake}"),
        (true, None) => format!("{base}|fake"),
        (false, None) => base,
    };
    gpu::sha256_hex(salted.as_bytes())
}

/// Refuses configurations the platform will not accept, with a clear fix
/// (SRS FR-50).
fn preflight(gpus: &[gpu::GpuInfo]) -> Result<()> {
    if gpus.is_empty() {
        bail!("no supported GPU found. NVIDIA GPUs need the driver installed (it provides nvidia-smi); Apple Silicon is detected automatically.");
    }
    for g in gpus {
        if g.model.to_uppercase().contains("NVIDIA") {
            let major: u32 = g
                .driver_version
                .split('.')
                .next()
                .and_then(|m| m.parse().ok())
                .unwrap_or(0);
            if major > 0 && major < MIN_NVIDIA_DRIVER {
                bail!(
                    "NVIDIA driver {} is too old; install R{MIN_NVIDIA_DRIVER} or newer (e.g. `sudo apt install nvidia-driver-550`).",
                    g.driver_version
                );
            }
        }
    }
    Ok(())
}

#[tokio::main]
async fn main() -> Result<()> {
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    fmt()
        .with_env_filter(filter)
        .with_target(false)
        .with_ansi(session::colour())
        .compact()
        .init();

    let args = Args::parse();
    let mut coordinator = args.coordinator.clone();
    if coordinator.ends_with(":8083") {
        // 8083 is the coordinator's HTTP port; agents speak gRPC on 50051.
        coordinator = coordinator.replace(":8083", ":50051");
    }

    let mut data_dir = args
        .data_dir
        .clone()
        .unwrap_or_else(|| home_dir().join(".ayeusann"));
    if let Some(i) = &args.instance {
        data_dir = data_dir.join(format!("instance-{i}"));
    }
    if args.reset {
        state::clear(&data_dir);
        info!("stored credential cleared");
    }

    let kind = runtime::Kind::parse(&args.runtime)?;
    let runtime_url = args
        .runtime_url
        .clone()
        .unwrap_or_else(|| kind.default_url().to_string());
    let rt = runtime::Runtime::new(kind, &runtime_url)?;

    info!(version = env!("CARGO_PKG_VERSION"), coordinator = %coordinator, runtime = kind.name(), "starting host agent");
    if args.fake_gpu {
        warn!("FAKE GPU MODE: reporting simulated hardware. Development only.");
    }

    let mut gpus = GpuDetector::new(args.fake_gpu).detect();
    if let Some(i) = &args.instance {
        for g in &mut gpus {
            g.uuid = format!("{}-instance-{i}", g.uuid);
        }
    }
    if let Err(e) = preflight(&gpus) {
        error!("{e}");
        std::process::exit(2);
    }
    for g in &gpus {
        info!(model = %g.model, vram_gb = g.vram_gb, driver = %g.driver_version, "GPU detected");
    }

    if !rt.healthy().await {
        warn!(
            "{} is not reachable at {}. The host will connect but receive no jobs until it is running{}",
            kind.name(),
            runtime_url,
            if kind == runtime::Kind::Ollama { " (install from https://ollama.com, then `ollama serve`)." } else { "." }
        );
    }

    let fp = fingerprint(&gpus, args.fake_gpu, args.instance.as_deref());
    let bench = BenchmarkSuite::new(args.fake_gpu, &coordinator, &fp).run_all();
    let (_wg_private, wg_public) = MeshManager::generate_keypair();

    let facts = HostFacts {
        hostname: hostname(),
        os: os_name(),
        os_version: sysinfo::System::os_version().unwrap_or_default(),
        kernel: sysinfo::System::kernel_version().unwrap_or_default(),
        region: args.region.clone(),
        gpus,
        fingerprint: fp,
        wg_public_key: wg_public,
        benchmark: bench,
    };

    let shared = Arc::new(Shared::default());
    let mut backoff = Duration::from_secs(1);
    let run = async {
        loop {
            let s = Session {
                coordinator_url: &coordinator,
                token: args.token.as_deref(),
                data_dir: data_dir.clone(),
                facts: &facts,
                runtime: rt.clone(),
                heartbeat: Duration::from_secs(args.heartbeat_interval.max(1)),
                shared: shared.clone(),
            };
            let started = std::time::Instant::now();
            match s.run().await {
                Outcome::Rejected(reason) => {
                    error!("registration rejected: {reason}");
                    return 1;
                }
                Outcome::Disconnected(e) => {
                    if started.elapsed() > Duration::from_secs(60) {
                        backoff = Duration::from_secs(1);
                    }
                    warn!(error = %format!("{e:#}"), retry_in_s = backoff.as_secs(), "disconnected from coordinator; reconnecting");
                    tokio::time::sleep(backoff).await;
                    backoff = (backoff * 2).min(Duration::from_secs(30));
                }
            }
        }
    };

    let code = tokio::select! {
        code = run => code,
        _ = tokio::signal::ctrl_c() => {
            info!("shutting down; the coordinator will move this host's jobs elsewhere");
            0
        }
    };
    std::process::exit(code);
}

#[cfg(test)]
mod tests {
    use super::*;

    fn nvidia(driver: &str) -> gpu::GpuInfo {
        gpu::GpuInfo {
            model: "NVIDIA GeForce RTX 4090".into(),
            vram_gb: 24,
            driver_version: driver.into(),
            cuda_version: String::new(),
            compute_capability_major: 8,
            compute_capability_minor: 9,
            uuid: "GPU-1".into(),
            fingerprint: "sha256:x".into(),
        }
    }

    #[test]
    fn preflight_enforces_driver_floor_and_a_gpu() {
        assert!(preflight(&[]).is_err());
        assert!(preflight(&[nvidia("470.82.01")]).is_err());
        assert!(preflight(&[nvidia("535.104.05")]).is_ok());
        assert!(preflight(&[nvidia("550.54.14")]).is_ok());
    }

    #[test]
    fn instances_get_distinct_fingerprints() {
        let g = vec![nvidia("550.1")];
        let a = fingerprint(&g, true, Some("a"));
        let b = fingerprint(&g, true, Some("b"));
        assert_ne!(a, b);
        assert_eq!(a, fingerprint(&g, true, Some("a")));
        assert_ne!(fingerprint(&g, true, None), fingerprint(&g, false, None));
    }
}
