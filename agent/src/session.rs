//! One coordinator session: register, heartbeat, and act on coordinator
//! messages (manifests, stops, inference requests) until the stream ends.

use crate::benchmark::BenchmarkReport;
use crate::gpu::GpuInfo;
use crate::output::{self, Out};
use crate::proto::agent_service_client::AgentServiceClient;
use crate::proto::{
    agent_message, coordinator_message, AgentMessage, BenchmarkReport as ProtoBenchmark,
    GpuBenchmark, GpuInfo as ProtoGpuInfo, Heartbeat, HeldReplica, ManifestDispatch,
    RegisterRequest, ReplicaState, StageEvent,
};
use crate::runtime::Runtime;
use crate::state;
use crate::{inference, manifest, telemetry};
use anyhow::{anyhow, Context, Result};
use base64::Engine;
use std::collections::HashMap;
use std::io::Write;
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
    /// What earlier agent versions called this machine.
    pub legacy_fingerprints: Vec<String>,
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

/// Tells the coordinator this agent can replace its own binary from a signed
/// release.
pub const CAP_SELF_UPDATE: &str = "self-update";

/// How long the coordinator has to answer a new session.
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(30);

/// Tells the coordinator this agent lists the replicas it still serves when it
/// registers, so a reconnect does not reload them.
pub const CAP_REPLICA_REPORT: &str = "replica-report";

/// State that survives reconnects: replicas stay loaded in the runtime while
/// the control connection blips.
#[derive(Default)]
pub struct Shared {
    replicas: Mutex<HashMap<String, Replica>>,
    /// Replicas being started: the task, and the model it is loading.
    starting: Mutex<HashMap<String, (AbortHandle, String)>>,
    inflight: Mutex<HashMap<String, AbortHandle>>,
    /// Models a start may have loaded before it was cut short. Nothing is
    /// serving them, so they are unloaded when the agent lets go of
    /// everything, unless a later start takes them over.
    loose: Mutex<std::collections::HashSet<String>>,
}

impl Shared {
    /// Stops the work that belonged to a connection that has ended. A replica
    /// that was still starting reports its progress down that connection, and
    /// a request in flight streams its answer down it; neither can finish
    /// usefully, and the coordinator sends the job again on the new one.
    /// Replicas that are serving are not touched.
    pub fn end_session(&self) {
        for (_, (h, model)) in self.starting.lock().unwrap().drain() {
            h.abort();
            // The job is normally sent again within seconds, so the model is
            // left where it is; it is only remembered in case it is not.
            self.loose.lock().unwrap().insert(model);
        }
        for (_, h) in self.inflight.lock().unwrap().drain() {
            h.abort();
        }
    }

    /// The replicas this agent can honestly say it is still serving. Each
    /// model is checked against the runtime first; a replica whose model is
    /// gone is forgotten, so the coordinator sends its job again.
    pub async fn held(&self, runtime: &Runtime) -> Vec<HeldReplica> {
        let snapshot: Vec<(String, String)> = self
            .replicas
            .lock()
            .unwrap()
            .iter()
            .filter(|(_, r)| r.serving)
            .map(|(id, r)| (id.clone(), r.runtime_model.clone()))
            .collect();
        let mut checked: HashMap<String, bool> = HashMap::new();
        let mut held = Vec::new();
        for (id, model) in snapshot {
            let ok = match checked.get(&model) {
                Some(ok) => *ok,
                None => {
                    let ok = runtime.still_serving(&model).await;
                    checked.insert(model.clone(), ok);
                    ok
                }
            };
            if ok {
                held.push(HeldReplica {
                    replica_id: id,
                    runtime_model: model,
                });
            } else {
                warn!(replica_id = %id, model = %model, "model is no longer loaded; the coordinator will send the job again");
                self.replicas.lock().unwrap().remove(&id);
            }
        }
        held
    }

    /// Whether this agent holds anything in the runtime for the platform.
    /// Requests being answered right now.
    pub fn busy(&self) -> usize {
        self.inflight.lock().unwrap().len()
    }

    pub fn has_replicas(&self) -> bool {
        !self.replicas.lock().unwrap().is_empty() || !self.loose.lock().unwrap().is_empty()
    }

    fn in_use(&self, model: &str) -> bool {
        self.replicas
            .lock()
            .unwrap()
            .values()
            .any(|r| r.runtime_model == model)
            || self
                .starting
                .lock()
                .unwrap()
                .values()
                .any(|(_, m)| m == model)
    }

    /// Frees everything this agent holds for the platform. Used when the
    /// platform has been unreachable for so long that holding a GPU's memory
    /// for it is no longer reasonable.
    pub async fn unload_all(&self, runtime: &Runtime) {
        self.end_session();
        let mut models: std::collections::HashSet<String> = self
            .replicas
            .lock()
            .unwrap()
            .drain()
            .map(|(_, r)| r.runtime_model)
            .collect();
        models.extend(self.loose.lock().unwrap().drain());
        for m in models {
            match runtime.unload(&m).await {
                Ok(()) => info!(model = %m, "unloaded"),
                Err(e) => warn!(model = %m, error = %format!("{e:#}"), "failed to unload"),
            }
        }
    }
}

/// Why a session ended, which decides whether to retry.
pub enum Outcome {
    /// Connection lost or closed; reconnect with backoff. `registered` tells a
    /// session that worked and then ended from an address that never answered.
    Disconnected {
        error: anyhow::Error,
        registered: bool,
    },
    /// The coordinator refused this host; retrying cannot help.
    Rejected(String),
}

pub struct Session<'a> {
    /// The address to dial for this attempt.
    pub coordinator_url: &'a str,
    /// The address this machine was configured with, which is what is saved.
    pub configured_url: &'a str,
    pub token: Option<&'a str>,
    pub data_dir: PathBuf,
    pub facts: &'a HostFacts,
    pub runtime: Runtime,
    pub heartbeat: Duration,
    pub shared: Arc<Shared>,
    pub fake_gpu: bool,
    /// Where update offers go. None when this agent does not update itself.
    pub updates: Option<tokio::sync::mpsc::Sender<crate::update::Offer>>,
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
        let registered = std::sync::atomic::AtomicBool::new(false);
        let outcome = match self.run_inner(&registered).await {
            Ok(o) => o,
            Err(error) => Outcome::Disconnected {
                error,
                registered: registered.load(std::sync::atomic::Ordering::Relaxed),
            },
        };
        // However it ended, nothing may keep talking into the old connection.
        self.shared.end_session();
        outcome
    }

    async fn run_inner(&self, registered: &std::sync::atomic::AtomicBool) -> Result<Outcome> {
        let mut st = state::load(&self.data_dir);
        let credential = st.host_credential.clone().filter(|c| !c.is_empty());
        if credential.is_none() && self.token.is_none() {
            return Ok(Outcome::Rejected(
                "this machine is not enrolled: pass --token <registration token> from the host console".into(),
            ));
        }

        let cached = self.runtime.cached_models().await.unwrap_or_default();
        // What is still loaded from before the connection dropped. Empty on a
        // fresh start, and the coordinator then sends every job again.
        let held = self.shared.held(&self.runtime).await;
        if !held.is_empty() {
            info!(
                replicas = held.len(),
                "still serving from before the connection dropped"
            );
        }
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
            agent_version: crate::update::VERSION.to_string(),
            hardware_fingerprint: self.facts.fingerprint.clone(),
            wg_public_key: self.facts.wg_public_key.clone(),
            runtime: self.runtime.kind.name().to_string(),
            cached_models: cached,
            replicas: held,
            capabilities: {
                let mut caps = vec![CAP_REPLICA_REPORT.to_string()];
                if self.updates.is_some() {
                    caps.push(CAP_SELF_UPDATE.to_string());
                }
                caps
            },
            legacy_fingerprints: self.facts.legacy_fingerprints.clone(),
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
        // A coordinator that accepts the connection and then says nothing (it
        // is frozen, or the proxy in front of it is waiting on it) must not
        // hold this attempt for ever: the loop that tries the next address,
        // and the one that gives the GPU back after too long, only run
        // between attempts.
        let mut inbound = tokio::time::timeout(
            HANDSHAKE_TIMEOUT,
            client.session(tokio_stream::wrappers::ReceiverStream::new(rx)),
        )
        .await
        .map_err(|_| anyhow!("the coordinator did not answer within {HANDSHAKE_TIMEOUT:?}"))?
        .context("opening session")?
        .into_inner();

        // ── Registration ─────────────────────────────────────
        let first = tokio::time::timeout(HANDSHAKE_TIMEOUT, inbound.message())
            .await
            .map_err(|_| {
                anyhow!(
                    "the coordinator did not register this machine within {HANDSHAKE_TIMEOUT:?}"
                )
            })?;
        let resp = match first {
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
                            state::clear_enrolment(&self.data_dir);
                        }
                        Outcome::Rejected(reason)
                    }
                    _ => Outcome::Disconnected {
                        error: anyhow!(reason),
                        registered: false,
                    },
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
        // The release key is pinned at enrolment and only then. Enrolling is
        // something the machine's owner does, with a token; a reconnect is
        // not. If any later session could fill in a missing key, whoever
        // controlled the platform could name their own and sign this
        // machine's software from then on. So enrolment records what the
        // platform named, including "nothing", and afterwards the record is
        // only ever changed from this machine (--release-key, or enrolling
        // again).
        let offered = if resp.release_public_key.is_empty() {
            String::new()
        } else {
            base64::engine::general_purpose::STANDARD.encode(&resp.release_public_key)
        };
        if credential.is_none() {
            st.release_public_key = Some(offered);
        } else if !offered.is_empty() {
            match st.release_public_key.as_deref() {
                Some(pinned) if pinned == offered => {}
                Some("") | None => warn!("the platform publishes signed updates, but this machine was enrolled before it named a release key, so it will not accept them. To accept them, run the install command again, or start the agent once with --release-key <key>."),
                Some(_) => warn!("the platform now names a different release key; keeping the one pinned at enrolment, so its updates will be refused. Enrol again to accept the new key."),
            }
        }
        // Connected: an update that was waiting to prove itself has.
        if st
            .update
            .as_ref()
            .is_some_and(|u| u.to == crate::update::VERSION)
        {
            info!(version = crate::update::VERSION, "update confirmed");
            st.update = None;
            st.skip_version = None;
            st.skip_until = None;
        }
        if !resp.host_credential.is_empty() {
            st.host_credential = Some(resp.host_credential.clone());
        }
        st.host_id = Some(resp.host_id.clone());
        st.tier = Some(resp.tier.clone());
        st.coordinator_url = Some(self.configured_url.to_string());
        st.last_good_url = Some(self.coordinator_url.to_string());
        // The platform's list is for machines elsewhere. An agent on the
        // platform's own machine keeps to the loopback address it was given.
        if !resp.coordinator_urls.is_empty() && !state::is_loopback(self.configured_url) {
            st.coordinator_urls = resp.coordinator_urls.clone();
        }
        st.region = Some(self.facts.region.clone());
        st.runtime = Some(self.runtime.kind.name().to_string());
        st.runtime_url = Some(self.runtime.base_url().to_string());
        st.fake_gpu = Some(self.fake_gpu);
        st.pending_token = None;
        st.last_error = None;
        state::save(&self.data_dir, &st).context("saving host credential")?;
        registered.store(true, std::sync::atomic::Ordering::Relaxed);
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
                    info!(version = %u.version, "a newer agent is published");
                    if let Some(updates) = &self.updates {
                        // Handed to the updater; never waited on here.
                        let _ = updates.try_send(crate::update::Offer {
                            manifest: u.manifest,
                            signature: u.signature,
                            download_url: u.download_url,
                            connected_via: self.coordinator_url.to_string(),
                        });
                    }
                }
                _ => {}
            }
        };
        hb.abort();
        result.map(|_: ()| Outcome::Disconnected {
            error: anyhow!("session ended"),
            registered: true,
        })
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
        // Asked to start what is already serving: say so at once. Reloading a
        // model that is in memory helps nobody.
        let already = self
            .shared
            .replicas
            .lock()
            .unwrap()
            .get(&replica_id)
            .is_some_and(|r| r.serving && r.runtime_model == m.runtime_model);
        if already {
            info!(replica_id = %replica_id, "asked to start a replica that is already serving");
            stage(
                tx,
                &replica_id,
                ReplicaState::Serving,
                &format!("{} via {}", m.runtime_model, self.runtime.kind.name()),
                "",
            )
            .await;
            return;
        }
        // A second manifest for a replica that is still starting replaces the
        // first attempt rather than racing it.
        if let Some((h, _)) = self.shared.starting.lock().unwrap().remove(&replica_id) {
            h.abort();
        }
        info!(replica_id = %replica_id, model = %m.model_name, runtime_model = %m.runtime_model, "manifest verified; starting replica");

        let runtime = self.runtime.clone();
        let shared = self.shared.clone();
        let tx = tx.clone();
        let started_model = m.runtime_model.clone();
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

            shared.loose.lock().unwrap().remove(&model);
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
            .insert(replica_id, (task.abort_handle(), started_model));
    }

    async fn on_stop(&self, replica_id: String, tx: &Sender<AgentMessage>) {
        // The model to let go of: the one a start was loading when it was cut
        // short, or the one the replica was serving.
        let mut model = None;
        if let Some((h, m)) = self.shared.starting.lock().unwrap().remove(&replica_id) {
            h.abort();
            model = Some(m);
        }
        if let Some(r) = self.shared.replicas.lock().unwrap().remove(&replica_id) {
            model = Some(r.runtime_model);
        }
        if let Some(m) = model {
            if !self.shared.in_use(&m) {
                self.shared.loose.lock().unwrap().remove(&m);
                if let Err(e) = self.runtime.unload(&m).await {
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
        // Not on the first beat: registering has just checked and pinned
        // every model, and a stop for one of them may be on its way.
        if healthy && n > 0 && n.is_multiple_of(48) {
            let models: std::collections::HashSet<String> = shared
                .replicas
                .lock()
                .unwrap()
                .values()
                .map(|r| r.runtime_model.clone())
                .collect();
            for m in models {
                // Looked at again just before each pin: pinning a model that
                // was stopped a moment ago would load it back for nobody.
                if shared.in_use(&m) {
                    let _ = runtime.pin(&m).await;
                }
            }
            // A model left loaded by a start that was cut short, which no job
            // has come back for in the minutes since, is let go.
            let stray: Vec<String> = shared.loose.lock().unwrap().drain().collect();
            for m in stray {
                if !shared.in_use(&m) {
                    info!(model = %m, "unloading a model no job came back for");
                    let _ = runtime.unload(&m).await;
                }
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

fn print_banner(f: &HostFacts, tier: &str, status: &str, runtime: &Runtime) {
    let gpu = f
        .gpus
        .first()
        .map(|g| format!("{} ({} GB)", g.model, g.vram_gb))
        .unwrap_or_else(|| "no GPU".into());
    let title = if output::colour() {
        "  \x1b[1;32m●\x1b[0m \x1b[1mHost online\x1b[0m"
    } else {
        "  * Host online"
    };
    let mut lines = vec![
        String::new(),
        title.to_string(),
        format!("    GPU       {gpu}"),
        format!("    Tier      {}   status: {status}", tier.to_uppercase()),
        format!(
            "    Runtime   {} at {}",
            runtime.kind.name(),
            runtime.base_url()
        ),
        format!("    Region    {}", f.region),
        String::new(),
    ];
    if !output::is_file() {
        lines.push(
            "    Keep this running to stay online. Track its jobs in the console under Hosts."
                .into(),
        );
        lines.push("    Press Ctrl+C to stop; running jobs are moved to other hosts.".into());
        lines.push(
            "    To keep it running in the background instead: ayeusann-agent service install"
                .into(),
        );
        lines.push(String::new());
    }
    let _ = writeln!(Out, "{}", lines.join("\n"));
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::Kind;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    /// A stand-in for Ollama's `/api/generate`: answers 200 unless the request
    /// names the model "gone:1", and records every body it was sent.
    async fn stub_runtime() -> (Runtime, Arc<Mutex<Vec<String>>>) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let seen = Arc::new(Mutex::new(Vec::new()));
        let log = seen.clone();
        tokio::spawn(async move {
            loop {
                let Ok((mut sock, _)) = listener.accept().await else {
                    return;
                };
                let log = log.clone();
                tokio::spawn(async move {
                    let mut buf = Vec::new();
                    let mut chunk = [0u8; 4096];
                    let body = loop {
                        let Ok(n) = sock.read(&mut chunk).await else {
                            return;
                        };
                        if n == 0 {
                            return;
                        }
                        buf.extend_from_slice(&chunk[..n]);
                        let text = String::from_utf8_lossy(&buf).to_string();
                        if let Some((head, body)) = text.split_once("\r\n\r\n") {
                            let len = head
                                .lines()
                                .find_map(|l| {
                                    l.to_ascii_lowercase()
                                        .strip_prefix("content-length:")
                                        .map(|v| v.trim().parse::<usize>().unwrap_or(0))
                                })
                                .unwrap_or(0);
                            if body.len() >= len {
                                break body.to_string();
                            }
                        }
                    };
                    let status = if body.contains("gone:1") {
                        "404 Not Found"
                    } else {
                        "200 OK"
                    };
                    log.lock().unwrap().push(body);
                    let _ = sock
                        .write_all(
                            format!("HTTP/1.1 {status}\r\ncontent-length: 2\r\nconnection: close\r\n\r\n{{}}")
                                .as_bytes(),
                        )
                        .await;
                });
            }
        });
        (
            Runtime::new(Kind::Ollama, &format!("http://{addr}")).unwrap(),
            seen,
        )
    }

    fn serving(shared: &Shared, id: &str, model: &str) {
        shared.replicas.lock().unwrap().insert(
            id.to_string(),
            Replica {
                runtime_model: model.to_string(),
                model_name: model.to_string(),
                serving: true,
            },
        );
    }

    #[tokio::test]
    async fn ending_a_session_stops_its_work_and_keeps_what_is_serving() {
        let shared = Shared::default();
        serving(&shared, "r-serving", "kept:1");
        let starting = tokio::spawn(tokio::time::sleep(Duration::from_secs(3600)));
        let request = tokio::spawn(tokio::time::sleep(Duration::from_secs(3600)));
        shared.starting.lock().unwrap().insert(
            "r-starting".into(),
            (starting.abort_handle(), "half:1".into()),
        );
        shared
            .inflight
            .lock()
            .unwrap()
            .insert("req-1".into(), request.abort_handle());

        shared.end_session();

        assert!(starting.await.unwrap_err().is_cancelled());
        assert!(request.await.unwrap_err().is_cancelled());
        assert!(shared.starting.lock().unwrap().is_empty());
        assert!(shared.inflight.lock().unwrap().is_empty());
        assert!(
            shared.replicas.lock().unwrap().contains_key("r-serving"),
            "a serving replica must survive the end of a connection"
        );
        // The model the cut-short start may have loaded is remembered.
        assert!(shared.loose.lock().unwrap().contains("half:1"));
        assert!(shared.has_replicas());
    }

    #[tokio::test]
    async fn only_replicas_the_runtime_still_has_are_reported() {
        let (runtime, seen) = stub_runtime().await;
        let shared = Shared::default();
        serving(&shared, "r-1", "kept:1");
        serving(&shared, "r-2", "kept:1");
        serving(&shared, "r-3", "gone:1");

        let mut held: Vec<String> = shared
            .held(&runtime)
            .await
            .into_iter()
            .map(|h| format!("{}={}", h.replica_id, h.runtime_model))
            .collect();
        held.sort();

        assert_eq!(held, vec!["r-1=kept:1", "r-2=kept:1"]);
        // The one whose model is gone is forgotten, so its job is sent again.
        assert!(!shared.replicas.lock().unwrap().contains_key("r-3"));
        assert_eq!(shared.replicas.lock().unwrap().len(), 2);
        // Each model is asked about once, and the asking pins it.
        let seen = seen.lock().unwrap();
        assert_eq!(seen.len(), 2, "one check per distinct model: {seen:?}");
        assert!(seen.iter().all(|b| b.contains("\"keep_alive\":-1")));
    }

    #[tokio::test]
    async fn nothing_is_reported_when_the_runtime_is_down() {
        let dead = Runtime::new(Kind::Ollama, "http://127.0.0.1:9").unwrap();
        let shared = Shared::default();
        serving(&shared, "r-1", "kept:1");
        assert!(shared.held(&dead).await.is_empty());
        assert!(!shared.has_replicas());
    }

    #[tokio::test]
    async fn an_orphaned_agent_unloads_every_model_once() {
        let (runtime, seen) = stub_runtime().await;
        let shared = Shared::default();
        serving(&shared, "r-1", "kept:1");
        serving(&shared, "r-2", "kept:1");
        serving(&shared, "r-3", "other:2");
        let starting = tokio::spawn(tokio::time::sleep(Duration::from_secs(3600)));
        shared
            .starting
            .lock()
            .unwrap()
            .insert("r-4".into(), (starting.abort_handle(), "half:1".into()));

        shared.unload_all(&runtime).await;

        assert!(!shared.has_replicas());
        assert!(starting.await.unwrap_err().is_cancelled());
        let mut seen = seen.lock().unwrap().clone();
        seen.sort();
        // Including the model a cut-short start may have left loaded.
        assert_eq!(seen.len(), 3, "one unload per distinct model: {seen:?}");
        assert!(seen.iter().all(|b| b.contains("\"keep_alive\":0")));
        for model in ["half:1", "kept:1", "other:2"] {
            assert!(
                seen.iter().any(|b| b.contains(model)),
                "{model} not unloaded"
            );
        }
    }
}
