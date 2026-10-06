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
mod update;

// Generated code: tonic returns `Status` by value, which newer clippy flags.
#[allow(clippy::result_large_err, clippy::large_enum_variant)]
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
#[command(name = "ayeusann-agent", version = update::VERSION, about)]
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

    /// Trust this key (base64) to sign agent updates. Only needed on a machine
    /// that was enrolled before its platform published signed updates; a
    /// machine enrolled since has the key already.
    #[arg(long, env = "SN_RELEASE_KEY", global = true)]
    release_key: Option<String>,
}

/// How long a background agent waits before asking again after the
/// coordinator refused it. A refusal is not fixed by retrying quickly; it is
/// fixed by `service install --token ...`, which restarts the agent anyway.
const REFUSED_RETRY: Duration = Duration::from_secs(15 * 60);

#[cfg(windows)]
extern "system" {
    fn FreeConsole() -> i32;
}

#[cfg(any(windows, target_os = "macos"))]
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

/// The operating system's own identifier for this installation: stable across
/// reboots, driver updates and hardware changes, and readable without
/// elevated rights.
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

#[cfg(target_os = "macos")]
fn machine_id() -> Option<String> {
    parse_ioreg_uuid(&command_output(
        "ioreg",
        &["-rd1", "-c", "IOPlatformExpertDevice"],
    ))
}

#[cfg(not(any(windows, target_os = "macos")))]
fn machine_id() -> Option<String> {
    ["/etc/machine-id", "/var/lib/dbus/machine-id"]
        .iter()
        .find_map(|p| std::fs::read_to_string(p).ok())
        .map(|id| id.trim().to_string())
        .filter(|id| !id.is_empty())
}

/// Picks `"IOPlatformUUID" = "…"` out of `ioreg` output.
#[cfg(any(target_os = "macos", test))]
fn parse_ioreg_uuid(out: &str) -> Option<String> {
    out.lines()
        .find(|l| l.contains("IOPlatformUUID"))
        .and_then(|l| l.split('"').nth(3))
        .map(str::to_string)
        .filter(|id| !id.is_empty())
}

/// What agents up to 0.3 used as the machine id: the same on Windows and
/// Linux, and nothing on macOS, where they fell back to the host name.
fn legacy_machine_id() -> Option<String> {
    if cfg!(target_os = "macos") {
        None
    } else if cfg!(windows) {
        machine_id()
    } else {
        std::fs::read_to_string("/etc/machine-id")
            .ok()
            .map(|id| id.trim().to_string())
            .filter(|id| !id.is_empty())
    }
}

fn salted(base: String, fake: bool, instance: Option<&str>) -> String {
    match (fake, instance) {
        (_, Some(i)) => format!("{base}|instance:{i}|fake:{fake}"),
        (true, None) => format!("{base}|fake"),
        (false, None) => base,
    }
}

/// A stable identity for this machine. Two agents on one machine are the same
/// host unless --instance says otherwise (development only).
///
/// It is built from the operating system's machine id, not from the GPUs: a
/// driver update, a new card or a different slot order must not turn a
/// machine into a stranger. Only when the system has no machine id at all
/// does it fall back to the set of GPU ids, and then to the host name.
fn fingerprint(
    machine: Option<&str>,
    gpus: &[gpu::GpuInfo],
    fake: bool,
    instance: Option<&str>,
) -> String {
    let base = match machine {
        Some(id) => format!("v2|machine:{id}"),
        None if !fake && !gpus.is_empty() => {
            let mut ids: Vec<&str> = gpus.iter().map(|g| g.uuid.as_str()).collect();
            ids.sort_unstable();
            format!("v2|gpus:{}", ids.join(","))
        }
        None => format!("v2|hostname:{}", hostname()),
    };
    gpu::sha256_hex(salted(base, fake, instance).as_bytes())
}

/// The identity agents up to 0.3 computed for this machine, so a machine they
/// enrolled is recognised when it enrols again. It took the first GPU's
/// fingerprint, which for NVIDIA included the driver version: that is the
/// defect the current identity fixes, and it is reproduced here on purpose.
fn legacy_fingerprint(
    legacy_machine: Option<&str>,
    gpus: &[gpu::GpuInfo],
    fake: bool,
    instance: Option<&str>,
) -> String {
    let base = if let (false, Some(g)) = (fake, gpus.first()) {
        format!("host:{}", gpu::legacy_fingerprint(g, instance))
    } else if let Some(id) = legacy_machine {
        format!("machine:{id}")
    } else {
        format!("hostname:{}", hostname())
    };
    gpu::sha256_hex(salted(base, fake, instance).as_bytes())
}

/// What has just happened, as far as an unproven update is concerned.
#[derive(Clone, Copy, PartialEq)]
enum Sign {
    /// The process has started.
    Started,
    /// An attempt to connect failed, or the platform refused this version.
    Failed,
}

/// Looks after an update that was installed and has not yet proven itself.
///
/// Called as early as possible at every start, and after every attempt to
/// connect that fails. If this is the new version, it is given up on when it
/// has both had its time and really tried (several failed attempts), or when
/// it has been started over and over without ever connecting, which is what a
/// build that crashes looks like. The previous binary is then put back and
/// started. If this is the previous version, the one that failed is left
/// alone for a day before it may be tried again: the platform may have been
/// at fault rather than the build.
fn settle_update(data_dir: &std::path::Path, exe: &std::path::Path, sign: Sign) {
    let mut st = state::load(data_dir);
    let Some(mut pending) = st.update.clone() else {
        return;
    };
    let give_up = |st: &mut state::State, version: String| {
        st.skip_version = Some(version);
        st.skip_until = Some(update::now() + update::SKIP_FOR_SECS);
        st.update = None;
    };
    if pending.to != update::VERSION {
        warn!(tried = %pending.to, running = update::VERSION, "an update did not take; leaving that version alone for a day");
        give_up(&mut st, pending.to);
        let _ = state::save(data_dir, &st);
        return;
    }
    match sign {
        Sign::Started => pending.starts += 1,
        Sign::Failed => pending.failures += 1,
    }
    st.update = Some(pending.clone());
    let _ = state::save(data_dir, &st);

    let tried_long_enough = update::now() - pending.at >= update::CONFIRM_WITHIN_SECS
        && pending.failures >= update::CONFIRM_FAILURES;
    let crash_loop = pending.starts >= update::CONFIRM_STARTS;
    if !tried_long_enough && !crash_loop {
        return;
    }
    error!(
        version = update::VERSION,
        previous = %pending.from,
        starts = pending.starts,
        failed_attempts = pending.failures,
        "this version has not been able to connect since it was installed; going back to the previous one"
    );
    match update::rollback(exe) {
        Ok(true) => {
            give_up(&mut st, pending.to);
            let _ = state::save(data_dir, &st);
            let e = update::restart(exe);
            error!(error = %e, "could not start the previous version; carrying on with this one");
        }
        Ok(false) => {
            // Nothing to go back to. Stop asking.
            st.update = None;
            let _ = state::save(data_dir, &st);
        }
        Err(e) => error!(error = %format!("{e:#}"), "could not restore the previous version"),
    }
}

/// Installs releases the platform offers. Runs beside the session: the
/// download and the checks happen while the agent keeps serving, and only the
/// last step, starting the new binary, interrupts anything.
async fn updater(
    mut offers: tokio::sync::mpsc::Receiver<update::Offer>,
    data_dir: PathBuf,
    exe: PathBuf,
    shared: Arc<Shared>,
) {
    use base64::Engine;
    while let Some(offer) = offers.recv().await {
        let mut st = state::load(&data_dir);
        let Some(key) = st
            .release_public_key
            .as_deref()
            .filter(|k| !k.is_empty())
            .and_then(|k| base64::engine::general_purpose::STANDARD.decode(k).ok())
        else {
            warn!("an update was offered, but this machine has no release key pinned; ignoring it");
            continue;
        };
        let manifest = match update::verify(&key, &offer.manifest, &offer.signature) {
            Ok(m) => m,
            Err(e) => {
                error!(error = %format!("{e:#}"), "refusing an update");
                continue;
            }
        };
        if update::compare(&manifest.version, update::VERSION) != std::cmp::Ordering::Greater {
            continue;
        }
        // Never back past a version this machine has already installed.
        if st
            .highest_version
            .as_deref()
            .is_some_and(|h| update::compare(&manifest.version, h) == std::cmp::Ordering::Less)
        {
            warn!(version = %manifest.version, newest_seen = ?st.highest_version, "refusing a release older than one this machine has already installed");
            continue;
        }
        if st.skip_version.as_deref() == Some(manifest.version.as_str()) {
            if st.skip_until.is_some_and(|t| update::now() < t) {
                info!(version = %manifest.version, "not installing a version that was tried and put back; it is tried again later");
                continue;
            }
            st.skip_version = None;
            st.skip_until = None;
            let _ = state::save(&data_dir, &st);
        }
        if st.update.is_some() {
            continue; // one at a time; the installed one has to prove itself first
        }

        info!(from = update::VERSION, to = %manifest.version, "downloading the new version");
        let staged = match update::fetch(&offer, &manifest, &exe).await {
            Ok(p) => p,
            Err(e) => {
                error!(error = %format!("{e:#}"), version = %manifest.version, "update not installed");
                continue;
            }
        };

        // Let answers in progress finish, within reason.
        for _ in 0..60 {
            if shared.busy() == 0 {
                break;
            }
            tokio::time::sleep(Duration::from_secs(1)).await;
        }
        let mut st = state::load(&data_dir);
        st.update = Some(update::Pending {
            from: update::VERSION.to_string(),
            to: manifest.version.clone(),
            at: update::now(),
            starts: 0,
            failures: 0,
        });
        st.highest_version = Some(manifest.version.clone());
        if let Err(e) = state::save(&data_dir, &st) {
            error!(error = %format!("{e:#}"), "could not record the update; not installing it");
            let _ = std::fs::remove_file(&staged);
            continue;
        }
        // Another agent sharing this binary may have put the new version in
        // place already. Swapping again would throw away the kept previous
        // version; starting what is there is all that is left to do.
        if update::reported_version(&exe).await.as_deref() == Some(manifest.version.as_str()) {
            let _ = std::fs::remove_file(&staged);
        } else if let Err(e) = update::swap(&staged, &exe) {
            error!(error = %format!("{e:#}"), "update not installed");
            st.update = None;
            let _ = state::save(&data_dir, &st);
            continue;
        }
        info!(version = %manifest.version, "new version installed; starting it");
        let e = update::restart(&exe);
        // Still here: the new binary could not be started. Put the old one back.
        error!(error = %e, "could not start the new version; restoring this one");
        let _ = update::rollback(&exe);
        st.update = None;
        st.skip_version = Some(manifest.version);
        st.skip_until = Some(update::now() + update::SKIP_FOR_SECS);
        let _ = state::save(&data_dir, &st);
    }
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

    // Before anything that can fail, exit or hang: a new version that breaks
    // during start-up (hardware detection, the benchmark) must still be able
    // to put its predecessor back.
    let exe = std::env::current_exe().ok();
    if let Some(exe) = &exe {
        settle_update(&data_dir, exe, Sign::Started);
    }
    if let Some(key) = args
        .release_key
        .as_deref()
        .map(str::trim)
        .filter(|k| !k.is_empty())
    {
        use base64::Engine;
        match base64::engine::general_purpose::STANDARD.decode(key) {
            Ok(k) if k.len() == 32 => {
                let mut st = state::load(&data_dir);
                if st.release_public_key.as_deref() != Some(key) {
                    st.release_public_key = Some(key.to_string());
                    let _ = state::save(&data_dir, &st);
                    info!("release key recorded; updates signed with it are accepted from now on");
                }
            }
            _ => warn!("--release-key is not a release key (base64 of 32 bytes); ignoring it"),
        }
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

    info!(version = update::VERSION, coordinator = %coordinator, runtime = kind.name(), "starting host agent");
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

    let machine = machine_id();
    if machine.is_none() {
        warn!("this system has no machine id; identifying the machine by its GPUs instead");
    }
    let fp = fingerprint(
        machine.as_deref(),
        &gpus,
        fake_gpu,
        args.instance.as_deref(),
    );
    let legacy_fp = legacy_fingerprint(
        legacy_machine_id().as_deref(),
        &gpus,
        fake_gpu,
        args.instance.as_deref(),
    );
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
        legacy_fingerprints: vec![legacy_fp],
        wg_public_key: wg_public,
        benchmark: bench,
    };

    let shared = Arc::new(Shared::default());

    // ── Self-update ──────────────────────────────────────────
    // A background agent replaces its own binary when the platform publishes
    // a release signed by the key pinned at enrolment. One started by hand in
    // a terminal is left alone unless asked (SN_SELF_UPDATE=1): it is usually
    // a build someone is working on.
    let asked = std::env::var("SN_SELF_UPDATE")
        .ok()
        .map(|v| v.trim().to_ascii_lowercase());
    let self_update = exe.is_some()
        && match asked.as_deref() {
            Some("1") | Some("true") | Some("yes") | Some("on") => true,
            Some("0") | Some("false") | Some("no") | Some("off") => false,
            Some("") | None => args.background,
            Some(other) => {
                warn!(
                    value = other,
                    "SN_SELF_UPDATE is not 1 or 0; using the default"
                );
                args.background
            }
        };
    let updates = if self_update {
        let (tx, rx) = tokio::sync::mpsc::channel::<update::Offer>(4);
        tokio::spawn(updater(
            rx,
            data_dir.clone(),
            exe.clone().unwrap(),
            shared.clone(),
        ));
        Some(tx)
    } else {
        None
    };
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
                updates: updates.clone(),
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
                    // A new version the platform refuses is as unproven as
                    // one that cannot reach it.
                    if let Some(exe) = &exe {
                        settle_update(&data_dir, exe, Sign::Failed);
                    }
                    // A platform that has refused this machine will not use
                    // what it has loaded; the GPU's memory goes back now.
                    if shared.has_replicas() {
                        shared.unload_all(&rt).await;
                    }
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
                    // A new version that cannot connect is put back.
                    if !registered {
                        if let Some(exe) = &exe {
                            settle_update(&data_dir, exe, Sign::Failed);
                        }
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
        let m = Some("machine-1");
        let a = fingerprint(m, &g, true, Some("a"));
        let b = fingerprint(m, &g, true, Some("b"));
        assert_ne!(a, b);
        assert_eq!(a, fingerprint(m, &g, true, Some("a")));
        assert_ne!(
            fingerprint(m, &g, true, None),
            fingerprint(m, &g, false, None)
        );
    }

    fn card(uuid: &str, driver: &str) -> gpu::GpuInfo {
        let mut g = nvidia(driver);
        g.uuid = uuid.into();
        g
    }

    /// The defect this replaces: a routine driver update changed the machine's
    /// identity and locked it out of the network for good.
    #[test]
    fn a_machine_keeps_its_identity_through_driver_and_hardware_changes() {
        let m = Some("4c4c4544-0042-3510-8052-b7c04f334433");
        let before = fingerprint(m, &[card("GPU-a", "550.54.14")], false, None);

        let driver_update = fingerprint(m, &[card("GPU-a", "560.35.03")], false, None);
        let second_card = fingerprint(
            m,
            &[card("GPU-a", "560.35.03"), card("GPU-b", "560.35.03")],
            false,
            None,
        );
        let swapped_order = fingerprint(
            m,
            &[card("GPU-b", "560.35.03"), card("GPU-a", "560.35.03")],
            false,
            None,
        );
        let new_card = fingerprint(m, &[card("GPU-z", "560.35.03")], false, None);
        for (what, fp) in [
            ("a driver update", &driver_update),
            ("a second card", &second_card),
            ("a different slot order", &swapped_order),
            ("a replaced card", &new_card),
        ] {
            assert_eq!(&before, fp, "{what} changed the machine's identity");
        }

        // Another machine is another machine.
        assert_ne!(
            before,
            fingerprint(Some("other"), &[card("GPU-a", "550.54.14")], false, None)
        );
        assert!(before.starts_with("sha256:"));
    }

    /// Without a machine id the GPUs identify the machine: by their ids alone,
    /// in any order, whatever the driver.
    #[test]
    fn without_a_machine_id_the_gpu_ids_identify_the_machine() {
        let a = fingerprint(
            None,
            &[card("GPU-a", "550.1"), card("GPU-b", "550.1")],
            false,
            None,
        );
        let b = fingerprint(
            None,
            &[card("GPU-b", "560.2"), card("GPU-a", "560.2")],
            false,
            None,
        );
        assert_eq!(a, b);
        assert_ne!(a, fingerprint(None, &[card("GPU-a", "550.1")], false, None));
    }

    /// The old identity is still computed, exactly as 0.3 computed it, so a
    /// machine enrolled by an older agent is recognised.
    #[test]
    fn the_legacy_identity_is_what_older_agents_sent() {
        let g = card("GPU-a", "550.54.14");
        let old_gpu_fp = gpu::sha256_hex(b"GPU-a:550.54.14:24");
        let expected = gpu::sha256_hex(format!("host:{old_gpu_fp}").as_bytes());
        assert_eq!(
            legacy_fingerprint(Some("machine-1"), std::slice::from_ref(&g), false, None),
            expected
        );
        // It did depend on the driver, which is why it is only a legacy match.
        assert_ne!(
            legacy_fingerprint(
                Some("machine-1"),
                &[card("GPU-a", "560.35.03")],
                false,
                None
            ),
            expected
        );
        // Simulated hardware was identified by the machine and the instance.
        assert_eq!(
            legacy_fingerprint(Some("machine-1"), &[g], true, Some("a")),
            gpu::sha256_hex(b"machine:machine-1|instance:a|fake:true")
        );
        // With --instance the GPU's id carries a suffix by the time this runs;
        // 0.3 hashed the id as detected, before the suffix.
        let mut suffixed = card("GPU-a", "550.54.14");
        suffixed.uuid = "GPU-a-instance-dev".into();
        assert_eq!(
            legacy_fingerprint(Some("machine-1"), &[suffixed], false, Some("dev")),
            gpu::sha256_hex(format!("host:{old_gpu_fp}|instance:dev|fake:false").as_bytes())
        );
        // And never equals the current identity.
        assert_ne!(
            legacy_fingerprint(Some("machine-1"), &[card("GPU-a", "1")], false, None),
            fingerprint(Some("machine-1"), &[card("GPU-a", "1")], false, None)
        );
    }

    #[test]
    fn reads_the_platform_uuid_from_ioreg() {
        let out = r#"+-o J314sAP  <class IOPlatformExpertDevice, id 0x100000252, registered, matched, active, busy 0 (0 ms), retain 40>
    {
      "IOPlatformSerialNumber" = "XXXXXXXXXX"
      "IOPlatformUUID" = "A1B2C3D4-0000-1111-2222-333344445555"
      "manufacturer" = <"Apple Inc.">
    }"#;
        assert_eq!(
            parse_ioreg_uuid(out).as_deref(),
            Some("A1B2C3D4-0000-1111-2222-333344445555")
        );
        assert_eq!(parse_ioreg_uuid("nothing useful"), None);
    }
}
