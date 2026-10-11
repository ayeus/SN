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

/// Tells the coordinator this agent says "still working" during a request
/// when asked to, so a slow answer is not mistaken for a dead machine.
pub const CAP_INFERENCE_KEEPALIVE: &str = "inference-keepalive";

/// Tells the coordinator this agent accepts messages up to MAX_MESSAGE_BYTES.
pub const CAP_LARGE_MESSAGES: &str = "large-messages";

/// The largest message accepted from the coordinator: the platform's limit on
/// a request body (16 MB) plus room for the fields around it. The library's
/// default is 4 MB, and a message over the limit ends the whole session, so
/// one large prompt used to take the machine offline.
const MAX_MESSAGE_BYTES: usize = 17 << 20;

/// The most requests this machine answers at once, across all its models. The
/// platform's router stays well below it; this is the machine's own backstop.
const MAX_INFLIGHT: usize = 32;

/// How long a replica that has been told to stop waits for the requests it
/// is answering. Shorter than the coordinator's own 30 s wait for the stop to
/// be confirmed.
const STOP_DRAIN: Duration = Duration::from_secs(20);

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
    /// Models this agent downloaded itself, as opposed to finding them
    /// already on the machine.
    pulled: Mutex<std::collections::BTreeSet<String>>,
    /// Where the record of all this is kept (the data directory). None in
    /// tests.
    ledger: Option<PathBuf>,
    /// Why the machine is being given back to its owner right now, if it is.
    yielding: Mutex<Option<String>>,
}

impl Shared {
    /// State for an agent that keeps its record of models in `data_dir`.
    pub fn new(data_dir: &std::path::Path) -> Self {
        Shared {
            pulled: Mutex::new(state::load_ledger(data_dir).pulled),
            ledger: Some(data_dir.to_path_buf()),
            ..Default::default()
        }
    }

    /// Writes down what this agent holds in the runtime. Called whenever that
    /// changes, so the record is right even if the agent is killed.
    fn record(&self) {
        let Some(dir) = &self.ledger else {
            return;
        };
        let mut loaded: std::collections::BTreeSet<String> = self
            .replicas
            .lock()
            .unwrap()
            .values()
            .map(|r| r.runtime_model.clone())
            .collect();
        loaded.extend(
            self.starting
                .lock()
                .unwrap()
                .values()
                .map(|(_, m)| m.clone()),
        );
        loaded.extend(self.loose.lock().unwrap().iter().cloned());
        let ledger = state::Ledger {
            loaded,
            pulled: self.pulled.lock().unwrap().clone(),
        };
        if let Err(e) = state::save_ledger(dir, &ledger) {
            warn!(error = %format!("{e:#}"), "could not record which models are loaded");
        }
    }

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

    /// Records whether the machine is being given back to its owner. The
    /// next heartbeat tells the platform.
    pub fn set_yield(&self, reason: Option<String>) {
        *self.yielding.lock().unwrap() = reason;
    }

    pub fn yield_reason(&self) -> Option<String> {
        self.yielding.lock().unwrap().clone()
    }

    /// Whether this agent holds anything in the runtime for the platform.
    /// Requests being answered right now.
    pub fn busy(&self) -> usize {
        self.inflight.lock().unwrap().len()
    }

    /// Everything this agent is doing with the GPU right now: requests being
    /// answered and models being loaded. A GPU that is busy while this is
    /// zero is busy with somebody else's work.
    pub fn working(&self) -> usize {
        self.busy() + self.starting.lock().unwrap().len()
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
        self.record();
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
                let mut caps = vec![
                    CAP_REPLICA_REPORT.to_string(),
                    CAP_INFERENCE_KEEPALIVE.to_string(),
                    CAP_LARGE_MESSAGES.to_string(),
                ];
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
        )
        .max_decoding_message_size(MAX_MESSAGE_BYTES);
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
        // The platform stops sending work within a heartbeat of being told
        // the machine is needed; a job already on its way is turned down.
        if let Some(reason) = self.shared.yield_reason() {
            info!(replica_id = %replica_id, reason = %reason, "not starting a replica: the machine is being given back to its owner");
            stage(
                tx,
                &replica_id,
                ReplicaState::Failed,
                "",
                &format!("the machine's owner needs it ({reason})"),
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
        let fake_gpu = self.fake_gpu;
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
                shared.pulled.lock().unwrap().insert(model.clone());
                shared.record();
            }

            // Loading a model there is no room for fails slowly, or succeeds
            // by pushing the machine into swap. Better to say so now. A
            // model that is already in memory needs no room made for it.
            if !fake_gpu && m.memory_mb > 0 && !runtime.loaded_models().await.contains(&model) {
                if let Some(free) = crate::gpu::free_memory_mb(&m.gpu_uuid) {
                    if !crate::gpu::enough_memory(m.memory_mb, free) {
                        let why = format!(
                            "not enough free memory to load {model}: it needs about {:.1} GB and {:.1} GB is free. \
                             Close other applications that are using the GPU, or deploy a smaller model.",
                            m.memory_mb as f64 / 1024.0,
                            free as f64 / 1024.0
                        );
                        warn!(replica_id = %rid, "{why}");
                        stage(&tx, &rid, ReplicaState::Failed, "", &why).await;
                        shared.starting.lock().unwrap().remove(&rid);
                        shared.record();
                        return;
                    }
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
            shared.record();
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
        self.shared.record();
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
        // From here the replica takes no new requests. The ones it is
        // answering are given a little while to finish before the model
        // goes: somebody is waiting for each of them, and the platform has
        // already stopped sending more. Done off this loop, which has other
        // messages to read meanwhile.
        let shared = self.shared.clone();
        let runtime = self.runtime.clone();
        let tx = tx.clone();
        tokio::spawn(async move {
            let waited = std::time::Instant::now();
            while shared.busy() > 0 && waited.elapsed() < STOP_DRAIN {
                tokio::time::sleep(Duration::from_millis(250)).await;
            }
            if let Some(m) = model {
                if !shared.in_use(&m) {
                    shared.loose.lock().unwrap().remove(&m);
                    if let Err(e) = runtime.unload(&m).await {
                        warn!(error = %e, "failed to unload model");
                    }
                }
            }
            shared.record();
            info!(replica_id = %replica_id, "replica stopped");
            stage(
                &tx,
                &replica_id,
                ReplicaState::Stopped,
                "stopped on request",
                "",
            )
            .await;
        });
    }

    async fn on_inference(&self, req: crate::proto::InferenceRequest, tx: &Sender<AgentMessage>) {
        let refuse = |status: i32, error: &str| AgentMessage {
            payload: Some(agent_message::Payload::InferenceChunk(
                crate::proto::InferenceChunk {
                    request_id: req.request_id.clone(),
                    done: true,
                    status_code: status,
                    error: error.to_string(),
                    ..Default::default()
                },
            )),
        };
        if !inference::allowed_path(&req.path) {
            warn!(request_id = %req.request_id, path = %req.path, "refusing a request for a path that is not an inference path");
            let _ = tx
                .send(refuse(400, "this path is not served by hosts"))
                .await;
            return;
        }
        let replica = self
            .shared
            .replicas
            .lock()
            .unwrap()
            .get(&req.replica_id)
            .cloned();
        let Some(replica) = replica.filter(|r| r.serving) else {
            let _ = tx
                .send(refuse(503, "replica is not loaded on this host"))
                .await;
            return;
        };
        if self.shared.busy() >= MAX_INFLIGHT {
            let _ = tx
                .send(refuse(
                    503,
                    "this host is answering as many requests as it can",
                ))
                .await;
            return;
        }
        tracing::debug!(request_id = %req.request_id, replica_id = %req.replica_id, path = %req.path, "request received");
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
        let yield_reason = shared.yield_reason();
        let hb = Heartbeat {
            ts: Some(now_ts()),
            cpu_usage_pct: sys.global_cpu_info().cpu_usage() as f64,
            memory_usage_pct: mem,
            gpu_status: telemetry::nvidia_status(),
            active_jobs: active,
            host_user_active: false,
            cached_models: cached.clone(),
            runtime_healthy: healthy,
            yielding: yield_reason.is_some(),
            yield_reason: yield_reason.unwrap_or_default(),
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

/// How long the platform has to take back what the machine is running, once
/// told the owner needs it, before the agent lets go by itself.
const YIELD_PATIENCE: Duration = Duration::from_secs(30);

/// Watches whether the machine should be given back to its owner, for as
/// long as the agent runs. The platform is told through the heartbeat and
/// takes back what the machine is running; if it has not within
/// YIELD_PATIENCE (it is unreachable, or too old to know what the heartbeat
/// means) the agent frees the GPU itself, because the owner comes first.
///
/// With a simulated GPU there is no hardware to give back, so only an
/// explicit pause counts; the same is true under SN_YIELD_RULES=off. Tests
/// run agents on whatever machine is to hand, which may be a laptop on its
/// battery.
pub async fn watch_yield(data_dir: PathBuf, runtime: Runtime, shared: Arc<Shared>, fake_gpu: bool) {
    let pause_only =
        fake_gpu || std::env::var("SN_YIELD_RULES").is_ok_and(|v| v.eq_ignore_ascii_case("off"));
    // Measuring runs small system commands; on Windows one of them is slow.
    let every = Duration::from_secs(if cfg!(windows) { 15 } else { 5 });
    let mut steady = crate::yielding::Steady::default();
    let mut since: Option<std::time::Instant> = None;
    let mut released = false;
    loop {
        let mut rules = crate::yielding::load(&data_dir);
        if pause_only {
            rules = crate::yielding::Rules {
                on_battery: false,
                when_gpu_busy: false,
                when_active: false,
                hours: None,
                paused_until: rules.paused_until,
            };
        }
        let kind = runtime.kind.name();
        let own = shared.working();
        let measured = {
            let rules = rules.clone();
            tokio::task::spawn_blocking(move || crate::yielding::probe(&rules, kind, own)).await
        };
        if let Ok(probe) = measured {
            let before = steady.current().map(str::to_string);
            let now = steady.look(&rules, &probe);
            if now != before {
                match &now {
                    Some(reason) => info!(reason = %reason, "giving the machine back to its owner"),
                    None => info!("the machine is free again; taking work"),
                }
            }
            shared.set_yield(now.clone());
            if now.is_some() {
                let began = *since.get_or_insert_with(std::time::Instant::now);
                if !released && began.elapsed() >= YIELD_PATIENCE {
                    released = true;
                    if shared.has_replicas() {
                        warn!(
                            "the platform has not taken back this machine's models; unloading them"
                        );
                        shared.unload_all(&runtime).await;
                    }
                }
            } else {
                since = None;
                released = false;
            }
        }
        tokio::time::sleep(every).await;
    }
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

    /// A scratch directory that is removed when the test ends.
    struct Scratch(PathBuf);
    impl Scratch {
        fn new(name: &str) -> Self {
            let dir = std::env::temp_dir()
                .join(format!("ayeusann-session-{name}-{}", std::process::id()));
            let _ = std::fs::remove_dir_all(&dir);
            std::fs::create_dir_all(&dir).unwrap();
            Scratch(dir)
        }
    }
    impl Drop for Scratch {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    #[tokio::test]
    async fn what_the_agent_holds_is_written_down_and_survives_it() {
        let dir = Scratch::new("ledger");
        let (runtime, _) = stub_runtime().await;
        let shared = Shared::new(&dir.0);
        serving(&shared, "r-1", "kept:1");
        shared.pulled.lock().unwrap().insert("kept:1".into());
        shared.loose.lock().unwrap().insert("half:1".into());
        shared.record();

        // What a command that is not the agent would find.
        let found = state::load_ledger(&dir.0);
        assert_eq!(
            found.loaded.iter().collect::<Vec<_>>(),
            vec!["half:1", "kept:1"]
        );
        assert_eq!(found.pulled.iter().collect::<Vec<_>>(), vec!["kept:1"]);

        // Unloading clears what is in memory; a download is still a download.
        shared.unload_all(&runtime).await;
        let after = state::load_ledger(&dir.0);
        assert!(after.loaded.is_empty(), "{after:?}");
        assert_eq!(after.pulled.len(), 1);

        // A new agent in the same directory remembers what it downloaded.
        let next = Shared::new(&dir.0);
        assert!(next.pulled.lock().unwrap().contains("kept:1"));
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
