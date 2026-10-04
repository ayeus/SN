//! One coordinator session: register, heartbeat, and act on coordinator
//! messages (manifests, stops, inference requests) until the stream ends.

use crate::benchmark::BenchmarkReport;
use crate::gpu::GpuInfo;
use crate::proto::agent_service_client::AgentServiceClient;
use crate::proto::{
    agent_message, coordinator_message, AgentMessage, BenchmarkReport as ProtoBenchmark,
    GpuBenchmark, GpuInfo as ProtoGpuInfo, Heartbeat, ManifestDispatch, RegisterRequest,
    ReplicaState, StageEvent,
};
use crate::runtime::Runtime;
use crate::state;
use crate::{inference, manifest, telemetry};
use anyhow::{anyhow, Context, Result};
use base64::Engine;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::sync::mpsc::Sender;
use tokio::task::AbortHandle;
use tonic::transport::{Channel, ClientTlsConfig, Endpoint};
use tracing::{error, info, warn};

/// Static facts about this machine, gathered once at startup.
pub struct HostFacts {
    pub hostname: String,
    pub os: String,
    pub os_version: String,
    pub kernel: String,
    pub region: String,
    pub gpus: Vec<GpuInfo>,
    pub fingerprint: String,
    pub wg_public_key: String,
    pub benchmark: BenchmarkReport,
}

/// A replica this host is serving.
#[derive(Clone)]
struct Replica {
    runtime_model: String,
    model_name: String,
    serving: bool,
}

/// State that survives reconnects: replicas stay loaded in the runtime while
/// the control connection blips.
#[derive(Default)]
pub struct Shared {
    replicas: Mutex<HashMap<String, Replica>>,
    starting: Mutex<HashMap<String, AbortHandle>>,
    inflight: Mutex<HashMap<String, AbortHandle>>,
}

/// Why a session ended, which decides whether to retry.
pub enum Outcome {
    /// Connection lost or closed; reconnect with backoff.
    Disconnected(anyhow::Error),
    /// The coordinator refused this host; retrying cannot help.
    Rejected(String),
}

pub struct Session<'a> {
    pub coordinator_url: &'a str,
    pub token: Option<&'a str>,
    pub data_dir: PathBuf,
    pub facts: &'a HostFacts,
    pub runtime: Runtime,
    pub heartbeat: Duration,
    pub shared: Arc<Shared>,
}

async fn channel(url: &str) -> Result<Channel> {
    let mut ep = Endpoint::from_shared(url.to_string())?
        .connect_timeout(Duration::from_secs(10))
        .http2_keep_alive_interval(Duration::from_secs(20))
        .keep_alive_timeout(Duration::from_secs(10))
        .keep_alive_while_idle(true);
    if url.starts_with("https://") {
        ep = ep.tls_config(ClientTlsConfig::new().with_webpki_roots())?;
    }
    Ok(ep.connect().await?)
}

fn now_ts() -> prost_types::Timestamp {
    let d = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default();
    prost_types::Timestamp {
        seconds: d.as_secs() as i64,
        nanos: d.subsec_nanos() as i32,
    }
}

async fn stage(
    tx: &Sender<AgentMessage>,
    replica_id: &str,
    state: ReplicaState,
    detail: &str,
    err: &str,
) {
    let _ = tx
        .send(AgentMessage {
            payload: Some(agent_message::Payload::StageEvent(StageEvent {
                replica_id: replica_id.to_string(),
                job_id: replica_id.to_string(),
                state: state as i32,
                detail: detail.to_string(),
                error_message: err.to_string(),
                ts: Some(now_ts()),
                ..Default::default()
            })),
        })
        .await;
}

impl Session<'_> {
    pub async fn run(&self) -> Outcome {
        match self.run_inner().await {
            Ok(o) => o,
            Err(e) => Outcome::Disconnected(e),
        }
    }

    async fn run_inner(&self) -> Result<Outcome> {
        let mut st = state::load(&self.data_dir);
        let credential = st.host_credential.clone().filter(|c| !c.is_empty());
        if credential.is_none() && self.token.is_none() {
            return Ok(Outcome::Rejected(
                "this machine is not enrolled: pass --token <registration token> from the host console".into(),
            ));
        }

        let cached = self.runtime.cached_models().await.unwrap_or_default();
        let reg = RegisterRequest {
            registration_token: if credential.is_some() {
                String::new()
            } else {
                self.token.unwrap_or_default().to_string()
            },
            host_credential: credential.clone().unwrap_or_default(),
            hostname: self.facts.hostname.clone(),
            os: self.facts.os.clone(),
            os_version: self.facts.os_version.clone(),
            kernel: self.facts.kernel.clone(),
            region: self.facts.region.clone(),
            gpus: self.facts.gpus.iter().map(to_proto_gpu).collect(),
            agent_version: env!("CARGO_PKG_VERSION").to_string(),
            hardware_fingerprint: self.facts.fingerprint.clone(),
            wg_public_key: self.facts.wg_public_key.clone(),
            runtime: self.runtime.kind.name().to_string(),
            cached_models: cached,
        };

        let mut client = AgentServiceClient::new(
            channel(self.coordinator_url)
                .await
                .context("connecting to coordinator")?,
        );
        let (tx, rx) = tokio::sync::mpsc::channel::<AgentMessage>(256);
        tx.send(AgentMessage {
            payload: Some(agent_message::Payload::Register(reg)),
        })
        .await?;
        let mut inbound = client
            .session(tokio_stream::wrappers::ReceiverStream::new(rx))
            .await
            .context("opening session")?
            .into_inner();

        // ── Registration ─────────────────────────────────────
        let resp = match inbound.message().await {
            Ok(Some(msg)) => match msg.payload {
                Some(coordinator_message::Payload::RegisterResponse(r)) => r,
                other => {
                    return Err(anyhow!(
                        "unexpected first message from coordinator: {other:?}"
                    ))
                }
            },
            Ok(None) => return Err(anyhow!("coordinator closed the session before registering")),
            Err(status) => {
                let reason = status.message().to_string();
                return Ok(match status.code() {
                    tonic::Code::Unauthenticated
                    | tonic::Code::PermissionDenied
                    | tonic::Code::FailedPrecondition
                    | tonic::Code::InvalidArgument => {
                        if credential.is_some() && status.code() == tonic::Code::Unauthenticated {
                            state::clear(&self.data_dir);
                        }
                        Outcome::Rejected(reason)
                    }
                    _ => Outcome::Disconnected(anyhow!(reason)),
                });
            }
        };
        if !resp.accepted {
            return Ok(Outcome::Rejected(resp.rejection_reason));
        }

        // Pin the manifest key on first enrolment; refuse a different one later.
        let offered = base64::engine::general_purpose::STANDARD.encode(&resp.manifest_public_key);
        match &st.manifest_public_key {
            Some(pinned) if pinned != &offered => {
                return Ok(Outcome::Rejected(
                    "the coordinator's manifest signing key changed since enrolment; refusing to run its jobs. \
                     If this is expected, re-enrol with a new registration token."
                        .into(),
                ));
            }
            _ => st.manifest_public_key = Some(offered),
        }
        if !resp.host_credential.is_empty() {
            st.host_credential = Some(resp.host_credential.clone());
        }
        st.host_id = Some(resp.host_id.clone());
        st.tier = Some(resp.tier.clone());
        st.coordinator_url = Some(self.coordinator_url.to_string());
        state::save(&self.data_dir, &st).context("saving host credential")?;
        let pinned_key = resp.manifest_public_key.clone();

        info!(host_id = %resp.host_id, tier = %resp.tier, status = %resp.status, "registered with coordinator");
        print_banner(self.facts, &resp.tier, &resp.status, &self.runtime);

        // ── Benchmark: on enrolment, then weekly (PRD F-12) ─
        let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;
        if !resp.host_credential.is_empty()
            || st
                .last_benchmark_at
                .map(|t| now - t > 7 * 86_400)
                .unwrap_or(true)
        {
            tx.send(benchmark_message(self.facts)).await?;
            st.last_benchmark_at = Some(now);
            let _ = state::save(&self.data_dir, &st);
        }

        // ── Heartbeats ───────────────────────────────────────
        let hb = tokio::spawn(heartbeat_loop(
            tx.clone(),
            self.runtime.clone(),
            self.heartbeat,
            self.shared.clone(),
        ));

        // ── Coordinator messages ─────────────────────────────
        let result = loop {
            let msg = match inbound.message().await {
                Ok(Some(m)) => m,
                Ok(None) => break Err(anyhow!("coordinator closed the session")),
                Err(e) => break Err(anyhow!("session error: {}", e.message())),
            };
            match msg.payload {
                Some(coordinator_message::Payload::Manifest(m)) => {
                    self.on_manifest(m, &pinned_key, &tx).await
                }
                Some(coordinator_message::Payload::StopReplica(s)) => {
                    self.on_stop(s.replica_id, &tx).await
                }
                Some(coordinator_message::Payload::InferenceRequest(r)) => {
                    self.on_inference(r, &tx).await
                }
                Some(coordinator_message::Payload::InferenceCancel(c)) => {
                    if let Some(h) = self.shared.inflight.lock().unwrap().remove(&c.request_id) {
                        h.abort();
                    }
                }
                Some(coordinator_message::Payload::Drain(d)) => {
                    warn!(reason = %d.reason, "coordinator requested drain");
                }
                Some(coordinator_message::Payload::Update(u)) => {
                    info!(version = %u.version, "agent update available");
                }
                _ => {}
            }
        };
        hb.abort();
        result.map(|_: ()| Outcome::Disconnected(anyhow!("session ended")))
    }

    async fn on_manifest(&self, m: ManifestDispatch, key: &[u8], tx: &Sender<AgentMessage>) {
        let replica_id = m.replica_id.clone();
        if let Err(e) = manifest::verify(key, &m) {
            error!(replica_id = %replica_id, error = %e, "refusing manifest");
            stage(
                tx,
                &replica_id,
                ReplicaState::Failed,
                "",
                &format!("manifest rejected: {e}"),
            )
            .await;
            return;
        }
        info!(replica_id = %replica_id, model = %m.model_name, runtime_model = %m.runtime_model, "manifest verified; starting replica");

        let runtime = self.runtime.clone();
        let shared = self.shared.clone();
        let tx = tx.clone();
        let task = tokio::spawn(async move {
            let rid = m.replica_id.clone();
            let model = m.runtime_model.clone();
            // Confirms receipt within the scheduler's reservation TTL (UML §8).
            stage(
                &tx,
                &rid,
                ReplicaState::Pulling,
                "checking local model cache",
                "",
            )
            .await;

            let cached = runtime
                .cached_models()
                .await
                .unwrap_or_default()
                .contains(&model);
            if !cached {
                let tx2 = tx.clone();
                let rid2 = rid.clone();
                let (ptx, mut prx) = tokio::sync::mpsc::unbounded_channel::<String>();
                let relay = tokio::spawn(async move {
                    while let Some(p) = prx.recv().await {
                        stage(&tx2, &rid2, ReplicaState::Pulling, &p, "").await;
                    }
                });
                let res = runtime
                    .pull(&model, |p| {
                        let _ = ptx.send(p);
                    })
                    .await;
                drop(ptx);
                let _ = relay.await;
                if let Err(e) = res {
                    stage(&tx, &rid, ReplicaState::Failed, "", &format!("{e:#}")).await;
                    shared.starting.lock().unwrap().remove(&rid);
                    return;
                }
            }

            stage(
                &tx,
                &rid,
                ReplicaState::Loading,
                "loading weights into memory",
                "",
            )
            .await;
            if let Err(e) = runtime.load(&model).await {
                stage(&tx, &rid, ReplicaState::Failed, "", &format!("{e:#}")).await;
                shared.starting.lock().unwrap().remove(&rid);
                return;
            }
            stage(
                &tx,
                &rid,
                ReplicaState::Warming,
                "running a warm-up generation",
                "",
            )
            .await;
            if let Err(e) = runtime.warm(&model).await {
                stage(&tx, &rid, ReplicaState::Failed, "", &format!("{e:#}")).await;
                shared.starting.lock().unwrap().remove(&rid);
                return;
            }

            shared.replicas.lock().unwrap().insert(
                rid.clone(),
                Replica {
                    runtime_model: model.clone(),
                    model_name: m.model_name.clone(),
                    serving: true,
                },
            );
            shared.starting.lock().unwrap().remove(&rid);
            stage(
                &tx,
                &rid,
                ReplicaState::Serving,
                &format!("{model} via {}", runtime.kind.name()),
                "",
            )
            .await;
            info!(replica_id = %rid, model = %model, "replica serving");
        });
        self.shared
            .starting
            .lock()
            .unwrap()
            .insert(replica_id, task.abort_handle());
    }

    async fn on_stop(&self, replica_id: String, tx: &Sender<AgentMessage>) {
        if let Some(h) = self.shared.starting.lock().unwrap().remove(&replica_id) {
            h.abort();
        }
        let removed = self.shared.replicas.lock().unwrap().remove(&replica_id);
        if let Some(r) = removed {
            let still_used = self
                .shared
                .replicas
                .lock()
                .unwrap()
                .values()
                .any(|x| x.runtime_model == r.runtime_model);
            if !still_used {
                if let Err(e) = self.runtime.unload(&r.runtime_model).await {
                    warn!(error = %e, "failed to unload model");
                }
            }
        }
        info!(replica_id = %replica_id, "replica stopped");
        stage(
            tx,
            &replica_id,
            ReplicaState::Stopped,
            "stopped on request",
            "",
        )
        .await;
    }

    async fn on_inference(&self, req: crate::proto::InferenceRequest, tx: &Sender<AgentMessage>) {
        let replica = self
            .shared
            .replicas
            .lock()
            .unwrap()
            .get(&req.replica_id)
            .cloned();
        let Some(replica) = replica.filter(|r| r.serving) else {
            let _ = tx
                .send(AgentMessage {
                    payload: Some(agent_message::Payload::InferenceChunk(
                        crate::proto::InferenceChunk {
                            request_id: req.request_id.clone(),
                            done: true,
                            status_code: 503,
                            error: "replica is not loaded on this host".into(),
                            ..Default::default()
                        },
                    )),
                })
                .await;
            return;
        };
        let id = req.request_id.clone();
        let shared = self.shared.clone();
        let runtime = self.runtime.clone();
        let tx = tx.clone();
        let id2 = id.clone();
        let task = tokio::spawn(async move {
            inference::run(runtime, req, replica.runtime_model, replica.model_name, tx).await;
            shared.inflight.lock().unwrap().remove(&id2);
        });
        self.shared
            .inflight
            .lock()
            .unwrap()
            .insert(id, task.abort_handle());
    }
}

async fn heartbeat_loop(
    tx: Sender<AgentMessage>,
    runtime: Runtime,
    every: Duration,
    shared: Arc<Shared>,
) {
    let mut sys = sysinfo::System::new();
    let mut tick = tokio::time::interval(every);
    let mut cached: Vec<String> = Vec::new();
    let mut n: u64 = 0;
    loop {
        tick.tick().await;
        sys.refresh_cpu();
        sys.refresh_memory();
        let mem = if sys.total_memory() > 0 {
            sys.used_memory() as f64 / sys.total_memory() as f64 * 100.0
        } else {
            0.0
        };
        let healthy = runtime.healthy().await;
        // The cache list changes rarely; refresh it every ~30 s.
        if healthy && n.is_multiple_of(6) {
            cached = runtime.cached_models().await.unwrap_or_default();
        }
        // Keep serving models resident (~every 4 min at a 5 s heartbeat).
        if healthy && n.is_multiple_of(48) {
            let models: Vec<String> = shared
                .replicas
                .lock()
                .unwrap()
                .values()
                .map(|r| r.runtime_model.clone())
                .collect();
            for m in models {
                let _ = runtime.pin(&m).await;
            }
        }
        n += 1;

        let active = shared.inflight.lock().unwrap().len() as i32;
        let hb = Heartbeat {
            ts: Some(now_ts()),
            cpu_usage_pct: sys.global_cpu_info().cpu_usage() as f64,
            memory_usage_pct: mem,
            gpu_status: telemetry::nvidia_status(),
            active_jobs: active,
            host_user_active: false,
            cached_models: cached.clone(),
            runtime_healthy: healthy,
        };
        if tx
            .send(AgentMessage {
                payload: Some(agent_message::Payload::Heartbeat(hb)),
            })
            .await
            .is_err()
        {
            return;
        }
    }
}

fn to_proto_gpu(g: &GpuInfo) -> ProtoGpuInfo {
    ProtoGpuInfo {
        model: g.model.clone(),
        vram_gb: g.vram_gb,
        driver_version: g.driver_version.clone(),
        cuda_version: g.cuda_version.clone(),
        compute_capability_major: g.compute_capability_major,
        compute_capability_minor: g.compute_capability_minor,
        uuid: g.uuid.clone(),
        fingerprint: g.fingerprint.clone(),
    }
}

fn benchmark_message(f: &HostFacts) -> AgentMessage {
    let r = &f.benchmark;
    AgentMessage {
        payload: Some(agent_message::Payload::Benchmark(ProtoBenchmark {
            hardware_fingerprint: f.fingerprint.clone(),
            gpu_benchmarks: f
                .gpus
                .iter()
                .map(|g| GpuBenchmark {
                    gpu_uuid: g.uuid.clone(),
                    compute_score: r.score_compute as f64,
                    vram_bandwidth_gbps: r.vram_bw_gbps as f64,
                })
                .collect(),
            disk_read_mbps: r.disk_read_mbps as f64,
            disk_write_mbps: r.disk_write_mbps as f64,
            net_upload_mbps: r.net_up_mbps as f64,
            net_download_mbps: r.net_down_mbps as f64,
            latency_to_pop_ms: r.latency_pop_ms as f64,
            ran_at: Some(now_ts()),
        })),
    }
}

/// Whether to colour terminal output. The classic Windows console prints
/// escape codes literally, and so does anything that is not a terminal.
pub fn colour() -> bool {
    use std::io::IsTerminal;
    !cfg!(windows) && std::io::stderr().is_terminal()
}

fn print_banner(f: &HostFacts, tier: &str, status: &str, runtime: &Runtime) {
    let gpu = f
        .gpus
        .first()
        .map(|g| format!("{} ({} GB)", g.model, g.vram_gb))
        .unwrap_or_else(|| "no GPU".into());
    eprintln!();
    if colour() {
        eprintln!("  \x1b[1;32m●\x1b[0m \x1b[1mHost online\x1b[0m");
    } else {
        eprintln!("  * Host online");
    }
    eprintln!("    GPU       {gpu}");
    eprintln!("    Tier      {}   status: {status}", tier.to_uppercase());
    eprintln!(
        "    Runtime   {} at {}",
        runtime.kind.name(),
        runtime.base_url()
    );
    eprintln!("    Region    {}", f.region);
    eprintln!();
    eprintln!("    Keep this running to stay online. Track its jobs in the console under Hosts.");
    eprintln!("    Press Ctrl+C to stop; running jobs are moved to other hosts.");
    eprintln!();
}
