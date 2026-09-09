mod benchmark;
mod gpu;
mod network;

pub mod proto {
    tonic::include_proto!("ayeus_ann.agent.v1");
}

use anyhow::Result;
use benchmark::BenchmarkSuite;
use clap::Parser;
use gpu::GpuDetector;
use network::MeshManager;
use proto::agent_service_client::AgentServiceClient;
use proto::{
    agent_message, coordinator_message, AgentMessage, GpuInfo as ProtoGpuInfo, Heartbeat,
    RegisterRequest,
};
use tracing::{error, info, warn};
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

    /// Coordinator endpoint (gRPC)
    #[arg(
        long,
        env = "SN_COORDINATOR_URL",
        default_value = "http://127.0.0.1:50051"
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

fn get_hostname() -> String {
    if let Ok(h) = std::env::var("HOSTNAME") {
        if !h.is_empty() {
            return h;
        }
    }
    if let Ok(out) = std::process::Command::new("hostname").output() {
        let s = String::from_utf8_lossy(&out.stdout).trim().to_string();
        if !s.is_empty() {
            return s;
        }
    }
    "ayeusann-host-node".to_string()
}

fn get_os_name() -> String {
    match std::env::consts::OS {
        "macos" => "macOS".to_string(),
        "linux" => "Linux".to_string(),
        "windows" => "Windows".to_string(),
        other => other.to_string(),
    }
}

fn get_os_version() -> String {
    if cfg!(target_os = "macos") {
        if let Ok(out) = std::process::Command::new("sw_vers")
            .arg("-productVersion")
            .output()
        {
            let ver = String::from_utf8_lossy(&out.stdout).trim().to_string();
            if !ver.is_empty() {
                return format!("macOS {}", ver);
            }
        }
    }
    if cfg!(target_os = "linux") {
        if let Ok(content) = std::fs::read_to_string("/etc/os-release") {
            for line in content.lines() {
                if let Some(stripped) = line.strip_prefix("PRETTY_NAME=") {
                    return stripped.trim_matches('"').to_string();
                }
            }
        }
    }
    std::env::consts::OS.to_string()
}

fn get_kernel() -> String {
    if let Ok(out) = std::process::Command::new("uname").arg("-r").output() {
        let k = String::from_utf8_lossy(&out.stdout).trim().to_string();
        if !k.is_empty() {
            return k;
        }
    }
    "unknown".to_string()
}

fn get_hardware_fingerprint(fake_gpu: bool, gpus: &[gpu::GpuInfo]) -> String {
    if fake_gpu {
        return "sha256:fake_host_hardware_fingerprint_01".to_string();
    }
    if !gpus.is_empty() && gpus[0].fingerprint.len() > 7 {
        return format!("sha256:host-{}", &gpus[0].fingerprint[7..]);
    }
    if let Ok(id) = std::fs::read_to_string("/etc/machine-id") {
        return format!("sha256:{}", id.trim());
    }
    format!("sha256:host-local-{}", get_hostname())
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

    let mut coord_url = args.coordinator_url.clone();
    if coord_url.ends_with(":8083") {
        coord_url = coord_url.replace(":8083", ":50051");
    }

    info!(
        service = "ayeusann-agent",
        version = env!("CARGO_PKG_VERSION"),
        coordinator_url = %coord_url,
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

    // 3. WireGuard Keypair Generation
    let (_priv_key, pub_key) = MeshManager::generate_keypair();

    info!(
        score_compute = report.score_compute,
        vram_bw_gbps = report.vram_bw_gbps,
        "hardware inspection and benchmark complete"
    );

    // 4. gRPC Connection to Coordinator
    if let Some(token) = args.token.as_deref().filter(|t| !t.is_empty()) {
        info!(
            coordinator_url = %coord_url,
            "connecting to coordinator gRPC session"
        );

        match connect_and_run_session(
            &coord_url,
            token,
            &gpus,
            &pub_key,
            &report,
            args.heartbeat_interval,
            args.fake_gpu,
        )
        .await
        {
            Ok(()) => {
                info!("gRPC session finished gracefully");
            }
            Err(e) => {
                error!(error = %e, "gRPC coordinator session ended with error");
            }
        }
    } else {
        warn!(
            "no registration token provided (pass --token <TOKEN> or set SN_REGISTRATION_TOKEN). Agent running in standalone inspection mode."
        );
        MeshManager::setup_overlay("10.200.0.10", &pub_key);

        info!("press Ctrl+C to stop agent");
        tokio::signal::ctrl_c().await?;
    }

    info!("received shutdown signal, stopping agent");
    Ok(())
}

async fn connect_and_run_session(
    coord_url: &str,
    token: &str,
    gpus: &[gpu::GpuInfo],
    pub_key: &str,
    report: &benchmark::BenchmarkReport,
    heartbeat_interval: u64,
    fake_gpu: bool,
) -> Result<()> {
    let hostname = get_hostname();
    let os_name = get_os_name();
    let os_version = get_os_version();
    let kernel = get_kernel();
    let hw_fingerprint = get_hardware_fingerprint(fake_gpu, gpus);

    let proto_gpus: Vec<ProtoGpuInfo> = gpus
        .iter()
        .map(|g| ProtoGpuInfo {
            model: g.model.clone(),
            vram_gb: g.vram_gb,
            driver_version: g.driver_version.clone(),
            cuda_version: g.cuda_version.clone(),
            compute_capability_major: g.compute_capability_major,
            compute_capability_minor: g.compute_capability_minor,
            uuid: g.uuid.clone(),
            fingerprint: g.fingerprint.clone(),
        })
        .collect();

    let reg_req = RegisterRequest {
        registration_token: token.to_string(),
        hostname: hostname.clone(),
        os: os_name,
        os_version,
        kernel,
        region: "IN-SOUTH".to_string(),
        gpus: proto_gpus.clone(),
        agent_version: env!("CARGO_PKG_VERSION").to_string(),
        hardware_fingerprint: hw_fingerprint,
        wg_public_key: pub_key.to_string(),
    };

    let mut client = AgentServiceClient::connect(coord_url.to_string()).await?;

    let (tx, rx) = tokio::sync::mpsc::channel(64);
    let request_stream = tokio_stream::wrappers::ReceiverStream::new(rx);

    // Send initial RegisterRequest
    tx.send(AgentMessage {
        payload: Some(agent_message::Payload::Register(reg_req)),
    })
    .await?;

    let response = client.session(request_stream).await?;
    let mut response_stream = response.into_inner();

    // Wait for RegisterResponse from coordinator
    if let Some(msg) = response_stream.message().await? {
        match msg.payload {
            Some(coordinator_message::Payload::RegisterResponse(reg_resp)) => {
                if !reg_resp.accepted {
                    anyhow::bail!(
                        "registration rejected by coordinator: {}",
                        reg_resp.rejection_reason
                    );
                }
                info!(
                    host_id = %reg_resp.host_id,
                    overlay_ip = %reg_resp.overlay_ip,
                    wg_endpoint = %reg_resp.wg_endpoint,
                    "host successfully registered with coordinator!"
                );
                MeshManager::setup_overlay(&reg_resp.overlay_ip, pub_key);
            }
            other => {
                anyhow::bail!("unexpected message before registration response: {:?}", other);
            }
        }
    } else {
        anyhow::bail!("coordinator closed session stream before registration response");
    }

    // Send BenchmarkReport
    let proto_benchmarks = gpus
        .iter()
        .map(|g| proto::GpuBenchmark {
            gpu_uuid: g.uuid.clone(),
            compute_score: report.score_compute as f64,
            vram_bandwidth_gbps: report.vram_bw_gbps as f64,
        })
        .collect();

    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap();

    let bench_msg = AgentMessage {
        payload: Some(agent_message::Payload::Benchmark(proto::BenchmarkReport {
            hardware_fingerprint: get_hardware_fingerprint(fake_gpu, gpus),
            gpu_benchmarks: proto_benchmarks,
            disk_read_mbps: report.disk_read_mbps as f64,
            disk_write_mbps: report.disk_write_mbps as f64,
            net_upload_mbps: report.net_up_mbps as f64,
            net_download_mbps: report.net_down_mbps as f64,
            latency_to_pop_ms: report.latency_pop_ms as f64,
            ran_at: Some(prost_types::Timestamp {
                seconds: now.as_secs() as i64,
                nanos: now.subsec_nanos() as i32,
            }),
        })),
    };
    let _ = tx.send(bench_msg).await;

    // Start background Heartbeat task
    let hb_tx = tx.clone();
    let hb_gpus = proto_gpus.clone();
    let mut hb_interval = tokio::time::interval(std::time::Duration::from_secs(heartbeat_interval));

    let heartbeat_handle = tokio::spawn(async move {
        loop {
            hb_interval.tick().await;
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap();
            let hb = AgentMessage {
                payload: Some(agent_message::Payload::Heartbeat(Heartbeat {
                    ts: Some(prost_types::Timestamp {
                        seconds: now.as_secs() as i64,
                        nanos: now.subsec_nanos() as i32,
                    }),
                    cpu_usage_pct: 12.5,
                    memory_usage_pct: 28.0,
                    gpu_status: hb_gpus
                        .iter()
                        .map(|g| proto::GpuStatus {
                            gpu_uuid: g.uuid.clone(),
                            utilization_pct: 0.0,
                            vram_used_mb: 256,
                            vram_total_mb: g.vram_gb * 1024,
                            temperature_c: 40,
                            power_draw_w: 95,
                        })
                        .collect(),
                    active_jobs: 0,
                    host_user_active: false,
                })),
            };
            if hb_tx.send(hb).await.is_err() {
                break;
            }
        }
    });

    // Process incoming coordinator messages
    info!("listening for coordinator manifest dispatches and instructions");
    tokio::select! {
        res = async {
            while let Some(msg) = response_stream.message().await? {
                match msg.payload {
                    Some(coordinator_message::Payload::Manifest(manifest)) => {
                        info!(
                            job_id = %manifest.job_id,
                            replica_id = %manifest.replica_id,
                            model_id = %manifest.model_id,
                            "received workload manifest dispatch from coordinator"
                        );
                    }
                    Some(coordinator_message::Payload::Drain(drain)) => {
                        warn!(reason = %drain.reason, "received drain request from coordinator");
                        let ack = AgentMessage {
                            payload: Some(agent_message::Payload::DrainAck(proto::DrainAck {
                                reason: drain.reason,
                                remaining_jobs: 0,
                            })),
                        };
                        let _ = tx.send(ack).await;
                    }
                    Some(coordinator_message::Payload::Update(update)) => {
                        info!(version = %update.version, "update available from coordinator");
                    }
                    _ => {}
                }
            }
            Ok::<(), anyhow::Error>(())
        } => {
            if let Err(e) = res {
                error!(error = %e, "stream error while receiving messages");
            }
        }
        _ = tokio::signal::ctrl_c() => {
            info!("received Ctrl+C in session, shutting down");
        }
    }

    heartbeat_handle.abort();
    Ok(())
}
