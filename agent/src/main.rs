mod benchmark;
mod gpu;
mod inference;
mod manifest;
mod network;
mod output;
mod runtime;
mod service;
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
use clap::{Parser, Subcommand};
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
///
/// With no subcommand it runs in this terminal until stopped. `service install`
/// runs it in the background instead and starts it again at every login.
#[derive(Parser, Debug)]
#[command(name = "ayeusann-agent", version, about)]
struct Cli {
    #[command(flatten)]
    args: Args,

    #[command(subcommand)]
    command: Option<Cmd>,
}

#[derive(Subcommand, Debug)]
enum Cmd {
    /// Run the agent in the background and start it at login.
    Service {
        #[command(subcommand)]
        action: ServiceAction,
    },
}

#[derive(Subcommand, Debug)]
enum ServiceAction {
    /// Set up the background service and start it. Takes the same options as
    /// a normal run (--token, --coordinator, --region, ...) and remembers them.
    Install,
    /// Stop the background agent and stop starting it at login. The machine
    /// stays enrolled.
    Uninstall,
    /// Show whether the background agent is set up and running.
    Status,
}

// Every option is global so it can be given before or after a subcommand.
// Options left out fall back to what the last enrolment used, then to the
// defaults named below.
#[derive(clap::Args, Debug)]
struct Args {
    /// One-time registration token from the host console. Only needed for the
    /// first run; afterwards the agent reconnects with its stored credential.
    #[arg(long, env = "SN_REGISTRATION_TOKEN", global = true)]
    token: Option<String>,

    /// Coordinator gRPC endpoint [default: the one last enrolled with, else
    /// http://127.0.0.1:50051].
    #[arg(
        long = "coordinator",
        visible_alias = "coordinator-url",
        env = "SN_COORDINATOR_URL",
        global = true
    )]
    coordinator: Option<String>,

    /// Region this machine is in [default: IN-SOUTH].
    #[arg(long, env = "SN_REGION", global = true)]
    region: Option<String>,

    /// Model runtime to drive: ollama or vllm [default: ollama].
    #[arg(long, env = "SN_RUNTIME", global = true)]
    runtime: Option<String>,

    /// Runtime base URL [default: the runtime's standard local port].
    #[arg(long, env = "SN_RUNTIME_URL", global = true)]
    runtime_url: Option<String>,

    /// Where the host credential and settings are stored [default: ~/.ayeusann].
    #[arg(long, env = "SN_DATA_DIR", global = true)]
    data_dir: Option<PathBuf>,

    /// Heartbeat interval in seconds (SRS FR-40: 5 s).
    #[arg(
        long,
        env = "SN_HEARTBEAT_INTERVAL",
        default_value = "5",
        global = true
    )]
    heartbeat_interval: u64,

    /// Development only: report a simulated RTX 4090 instead of real hardware.
    #[arg(long, env = "SN_FAKE_GPU", global = true)]
    fake_gpu: bool,

    /// Development only: run several agents on one machine as distinct hosts.
    #[arg(long, env = "SN_INSTANCE", hide = true, global = true)]
    instance: Option<String>,

    /// Forget the stored credential and enrol again with --token.
    #[arg(long, global = true)]
    reset: bool,

    /// Write output to this file instead of the terminal.
    #[arg(long, global = true)]
    log_file: Option<PathBuf>,

    /// Set by the background service: detach from any console window.
    #[arg(long, hide = true, global = true)]
    background: bool,
}

/// How long a background agent waits before asking again after the
/// coordinator refused it. A refusal is not fixed by retrying quickly; it is
/// fixed by `service install --token ...`, which restarts the agent anyway.
const REFUSED_RETRY: Duration = Duration::from_secs(15 * 60);

#[cfg(windows)]
extern "system" {
    fn FreeConsole() -> i32;
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
    let Cli { args, command } = Cli::parse();

    let base_dir = args
        .data_dir
        .clone()
        .unwrap_or_else(|| home_dir().join(".ayeusann"));
    let data_dir = match &args.instance {
        Some(i) => base_dir.join(format!("instance-{i}")),
        None => base_dir.clone(),
    };

    if let Some(Cmd::Service { action }) = command {
        let spec = service::Spec::new(&base_dir, &data_dir, args.instance.as_deref())?;
        let code = match action {
            ServiceAction::Install => service::install(
                &spec,
                service::Settings {
                    token: args.token,
                    coordinator: args.coordinator,
                    region: args.region,
                    runtime: args.runtime,
                    runtime_url: args.runtime_url,
                    fake_gpu: args.fake_gpu,
                    reset: args.reset,
                },
            ),
            ServiceAction::Uninstall => service::uninstall(&spec),
            ServiceAction::Status => service::status(&spec),
        };
        std::process::exit(code);
    }

    if let Some(path) = &args.log_file {
        output::to_file(path)?;
    }
    if args.background {
        // Started by the login item on Windows, the agent would otherwise sit
        // in a console window of its own.
        #[cfg(windows)]
        unsafe {
            FreeConsole();
        }
        let _ = std::fs::create_dir_all(&data_dir);
        let _ = std::fs::write(data_dir.join("agent.pid"), std::process::id().to_string());
    }

    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    fmt()
        .with_env_filter(filter)
        .with_target(false)
        .with_ansi(output::colour())
        .with_writer(|| output::Out)
        .compact()
        .init();

    if args.reset {
        state::clear(&data_dir);
        info!("stored credential cleared");
    }

    // Anything not given on the command line comes from the last enrolment, so
    // a restart needs no arguments.
    let saved = state::load(&data_dir);
    let mut coordinator = args
        .coordinator
        .clone()
        .or(saved.coordinator_url)
        .unwrap_or_else(|| "http://127.0.0.1:50051".into());
    if coordinator.ends_with(":8083") {
        // 8083 is the coordinator's HTTP port; agents speak gRPC on 50051.
        coordinator = coordinator.replace(":8083", ":50051");
    }
    let region = args
        .region
        .clone()
        .or(saved.region)
        .unwrap_or_else(|| "IN-SOUTH".into());
    let runtime_name = args
        .runtime
        .clone()
        .or(saved.runtime)
        .unwrap_or_else(|| "ollama".into());
    let fake_gpu = args.fake_gpu || saved.fake_gpu.unwrap_or(false);
    let token = args.token.clone().or(saved.pending_token);

    let kind = runtime::Kind::parse(&runtime_name)?;
    let runtime_url = args
        .runtime_url
        .clone()
        .or(saved.runtime_url)
        .unwrap_or_else(|| kind.default_url().to_string());
    let rt = runtime::Runtime::new(kind, &runtime_url)?;

    info!(version = env!("CARGO_PKG_VERSION"), coordinator = %coordinator, runtime = kind.name(), "starting host agent");
    if fake_gpu {
        warn!("FAKE GPU MODE: reporting simulated hardware. Development only.");
    }

    let mut gpus = GpuDetector::new(fake_gpu).detect();
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

    let fp = fingerprint(&gpus, fake_gpu, args.instance.as_deref());
    let bench = BenchmarkSuite::new(fake_gpu, &coordinator, &fp).run_all();
    let (_wg_private, wg_public) = MeshManager::generate_keypair();

    let facts = HostFacts {
        hostname: hostname(),
        os: os_name(),
        os_version: sysinfo::System::os_version().unwrap_or_default(),
        kernel: sysinfo::System::kernel_version().unwrap_or_default(),
        region,
        gpus,
        fingerprint: fp,
        wg_public_key: wg_public,
        benchmark: bench,
    };

    let shared = Arc::new(Shared::default());
    // How long models stay loaded for a platform that cannot be reached. A
    // blip or a restart is over in seconds; past this the GPU's memory goes
    // back to its owner, and jobs are sent again when the platform returns.
    let orphan_after = Duration::from_secs(
        std::env::var("SN_ORPHAN_UNLOAD_SECS")
            .ok()
            .and_then(|v| v.parse().ok())
            .unwrap_or(600),
    );
    let mut last_connected = std::time::Instant::now();
    let mut backoff = Duration::from_secs(1);
    // Which of the known addresses to dial. An address that never answers is
    // skipped for the next one; an address that worked is kept.
    let mut attempt = 0usize;
    let run = async {
        loop {
            let saved = state::load(&data_dir);
            let candidates = state::coordinator_candidates(
                saved.last_good_url.as_deref(),
                &coordinator,
                &saved.coordinator_urls,
            );
            let url = candidates[attempt % candidates.len()].clone();
            let s = Session {
                coordinator_url: &url,
                configured_url: &coordinator,
                token: token.as_deref(),
                data_dir: data_dir.clone(),
                facts: &facts,
                runtime: rt.clone(),
                heartbeat: Duration::from_secs(args.heartbeat_interval.max(1)),
                shared: shared.clone(),
                fake_gpu,
            };
            let started = std::time::Instant::now();
            match s.run().await {
                Outcome::Rejected(reason) => {
                    error!("registration rejected: {reason}");
                    // Recorded so `service install` and `service status` can
                    // say why the machine is not online.
                    let mut st = state::load(&data_dir);
                    st.last_error = Some(reason);
                    let _ = state::save(&data_dir, &st);
                    if !args.background {
                        return 1;
                    }
                    // Nobody is watching a background agent exit, and its
                    // supervisor would only start it straight back up.
                    tokio::time::sleep(REFUSED_RETRY).await;
                }
                Outcome::Disconnected {
                    error: e,
                    registered,
                } => {
                    if started.elapsed() > Duration::from_secs(60) {
                        backoff = Duration::from_secs(1);
                    }
                    if registered {
                        // It worked, so it is first in the list next time round.
                        attempt = 0;
                        last_connected = std::time::Instant::now();
                    } else {
                        attempt += 1;
                    }
                    if shared.has_replicas() && last_connected.elapsed() >= orphan_after {
                        warn!(
                            minutes = last_connected.elapsed().as_secs() / 60,
                            "the platform has been unreachable for too long; unloading its models"
                        );
                        shared.unload_all(&rt).await;
                    }
                    warn!(error = %format!("{e:#}"), address = %url, retry_in_s = backoff.as_secs(), "disconnected from coordinator; reconnecting");
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
