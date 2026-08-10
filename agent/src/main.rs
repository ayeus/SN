mod benchmark;
mod gpu;
mod network;

use anyhow::Result;
use benchmark::BenchmarkSuite;
use clap::Parser;
use gpu::GpuDetector;
use network::MeshManager;
use tracing::{info, warn};
use tracing_subscriber::{fmt, EnvFilter};

/// AyeusANN Host Agent
///
/// Manages GPU resources, executes workloads in isolation, and reports
/// health/usage to the AyeusANN coordinator.
#[derive(Parser, Debug)]
#[command(name = "ayeusann-agent", version, about)]
struct Args {
    /// Registration token (one-time, from host console)
    #[arg(long, env = "SN_REGISTRATION_TOKEN")]
    token: Option<String>,

    /// Coordinator endpoint
    #[arg(
        long,
        env = "SN_COORDINATOR_URL",
        default_value = "http://localhost:8083"
    )]
    coordinator_url: String,

    /// Agent data directory
    #[arg(long, env = "SN_DATA_DIR", default_value = "/var/lib/ayeusann")]
    data_dir: String,

    /// Heartbeat interval in seconds
    #[arg(long, env = "SN_HEARTBEAT_INTERVAL", default_value = "5")]
    heartbeat_interval: u64,

    /// Enable fake GPU mode for development
    #[arg(long, env = "SN_FAKE_GPU", default_value = "false")]
    fake_gpu: bool,
}

#[tokio::main]
async fn main() -> Result<()> {
    // Initialize structured JSON logging
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));

    fmt()
        .json()
        .with_env_filter(filter)
        .with_target(true)
        .with_thread_ids(true)
        .init();

    let args = Args::parse();

    info!(
        service = "ayeusann-agent",
        version = env!("CARGO_PKG_VERSION"),
        coordinator_url = %args.coordinator_url,
        fake_gpu = args.fake_gpu,
        "starting AyeusANN host agent"
    );

    if args.fake_gpu {
        warn!("running in FAKE GPU mode — no real GPU validation will occur");
    }

    // 1. Hardware GPU Detection
    let detector = GpuDetector::new(args.fake_gpu);
    let gpus = detector.detect();

    info!(gpu_count = gpus.len(), "detected GPU inventory on host");

    for gpu in &gpus {
        info!(
            model = %gpu.model,
            vram_gb = gpu.vram_gb,
            uuid = %gpu.uuid,
            "registered GPU device"
        );
    }

    // 2. Hardware Benchmark Suite Execution
    let bench = BenchmarkSuite::new(args.fake_gpu);
    let report = bench.run_all();

    // 3. WireGuard Keypair Generation & Network Mesh Configuration
    let (_priv_key, pub_key) = MeshManager::generate_keypair();
    MeshManager::setup_overlay("10.200.0.10", &pub_key);

    info!(
        score_compute = report.score_compute,
        vram_bw_gbps = report.vram_bw_gbps,
        "agent initialized successfully — ready for workloads"
    );

    // Keep running until signal
    tokio::signal::ctrl_c().await?;
    info!("received shutdown signal, stopping agent");

    Ok(())
}
