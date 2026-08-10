use anyhow::Result;
use clap::Parser;
use tracing::{info, warn};
use tracing_subscriber::{fmt, EnvFilter};

/// SpazeNode Host Agent
///
/// Manages GPU resources, executes workloads in isolation, and reports
/// health/usage to the SpazeNode coordinator.
#[derive(Parser, Debug)]
#[command(name = "spazenode-agent", version, about)]
struct Args {
    /// Registration token (one-time, from host console)
    #[arg(long, env = "SN_REGISTRATION_TOKEN")]
    token: Option<String>,

    /// Coordinator endpoint
    #[arg(long, env = "SN_COORDINATOR_URL", default_value = "http://localhost:8083")]
    coordinator_url: String,

    /// Agent data directory
    #[arg(long, env = "SN_DATA_DIR", default_value = "/var/lib/spazenode")]
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
    let filter = EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| EnvFilter::new("info"));

    fmt()
        .json()
        .with_env_filter(filter)
        .with_target(true)
        .with_thread_ids(true)
        .init();

    let args = Args::parse();

    info!(
        service = "spazenode-agent",
        version = env!("CARGO_PKG_VERSION"),
        coordinator_url = %args.coordinator_url,
        fake_gpu = args.fake_gpu,
        "starting spazenode host agent"
    );

    if args.fake_gpu {
        warn!("running in FAKE GPU mode — no real GPU validation will occur");
    }

    // Phase 0: Just verify the agent starts and logs correctly.
    // Phase 4 will add: GPU detection, registration, heartbeat, benchmarking.
    info!("agent initialized successfully (skeleton — awaiting Phase 4 implementation)");

    // Keep running until signal
    tokio::signal::ctrl_c().await?;
    info!("received shutdown signal, stopping agent");

    Ok(())
}
