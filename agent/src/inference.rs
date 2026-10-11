//! Executes tunnelled inference requests against the local runtime and streams
//! the answer back over the coordinator session (ADR-011).

use crate::proto::{agent_message, AgentMessage, InferenceChunk, InferenceRequest};
use crate::runtime::Runtime;
use futures_util::StreamExt;
use serde_json::Value;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio::sync::mpsc::Sender;
use tokio::time::Instant;

/// Upper bound on a non-streamed response body.
const MAX_RESPONSE_BYTES: usize = 32 << 20;

/// Token accounting for one request.
#[derive(Debug, Default, Clone, Copy, PartialEq)]
pub struct Usage {
    pub prompt: i32,
    pub completion: i32,
    /// True when the runtime reported exact counts.
    pub exact: bool,
}

/// Rewrites one server-sent event from the runtime for the customer:
///   * records exact token usage when the runtime reports it,
///   * replaces the runtime's model id with the catalogue name,
///   * drops the usage-only chunk the agent asked for unless the client asked
///     for it too (OpenAI clients index `choices[0]` and would crash on it).
///
/// Returns the bytes to forward, which may be empty.
pub fn rewrite_event(
    event: &str,
    model_name: &str,
    client_wants_usage: bool,
    usage: &mut Usage,
    content_chunks: &mut i32,
) -> String {
    let mut out = String::new();
    for line in event.lines() {
        let line = line.trim_end_matches('\r');
        let Some(data) = line.strip_prefix("data:") else {
            continue; // comments and other SSE fields carry nothing for clients
        };
        let data = data.trim_start();
        if data == "[DONE]" {
            out.push_str("data: [DONE]\n\n");
            continue;
        }
        let Ok(mut v) = serde_json::from_str::<Value>(data) else {
            out.push_str("data: ");
            out.push_str(data);
            out.push_str("\n\n");
            continue;
        };

        let has_usage = v.get("usage").map(|u| !u.is_null()).unwrap_or(false);
        if has_usage {
            usage.prompt = v["usage"]["prompt_tokens"].as_i64().unwrap_or(0) as i32;
            usage.completion = v["usage"]["completion_tokens"].as_i64().unwrap_or(0) as i32;
            usage.exact = true;
        }
        let no_choices = v["choices"]
            .as_array()
            .map(|a| a.is_empty())
            .unwrap_or(false);
        if !client_wants_usage {
            if has_usage && no_choices {
                continue;
            }
            if let Some(obj) = v.as_object_mut() {
                obj.remove("usage");
            }
        }
        if v["choices"][0]["delta"]["content"]
            .as_str()
            .map(|s| !s.is_empty())
            .unwrap_or(false)
        {
            *content_chunks += 1;
        }
        if v.get("model").is_some() {
            v["model"] = Value::String(model_name.to_string());
        }
        out.push_str("data: ");
        out.push_str(&v.to_string());
        out.push_str("\n\n");
    }
    out
}

/// Rough prompt size for runtimes that do not report usage: ~4 characters per
/// token, the usual English approximation.
pub fn estimate_prompt_tokens(body: &Value) -> i32 {
    let mut chars = 0usize;
    if let Some(msgs) = body["messages"].as_array() {
        for m in msgs {
            chars += m["content"].as_str().map(str::len).unwrap_or(0);
        }
    }
    chars += body["prompt"].as_str().map(str::len).unwrap_or(0);
    chars.div_ceil(4) as i32
}

/// Finds the end of the next SSE event (a blank line).
fn event_end(buf: &[u8]) -> Option<usize> {
    buf.windows(2).position(|w| w == b"\n\n").map(|p| p + 2)
}

/// The largest piece of an answer sent in one message. Anything bigger is
/// split, so no answer, however long, is one message large enough to be
/// refused by the coordinator or to hold up the heartbeats queued behind it.
const PIECE_BYTES: usize = 256 << 10;

/// Where one request's chunks go. It remembers when it last sent anything, so
/// the keep-alive only speaks when the request has been silent.
#[derive(Clone)]
struct Sink {
    tx: Sender<AgentMessage>,
    id: String,
    last: Arc<Mutex<Instant>>,
}

impl Sink {
    fn new(tx: Sender<AgentMessage>, id: &str) -> Self {
        Sink {
            tx,
            id: id.to_string(),
            last: Arc::new(Mutex::new(Instant::now())),
        }
    }

    async fn send(&self, mut chunk: InferenceChunk) {
        chunk.request_id = self.id.clone();
        *self.last.lock().unwrap() = Instant::now();
        let _ = self
            .tx
            .send(AgentMessage {
                payload: Some(agent_message::Payload::InferenceChunk(chunk)),
            })
            .await;
    }

    /// Sends `data` in pieces. `last` describes the final piece: whether it
    /// ends the request, and the usage that goes with it.
    async fn send_data(&self, data: &[u8], status: i32, last: InferenceChunk) {
        let pieces = split(data);
        let n = pieces.len();
        for (i, piece) in pieces.into_iter().enumerate() {
            let mut chunk = if i + 1 == n {
                last.clone()
            } else {
                InferenceChunk::default()
            };
            chunk.data = piece.to_vec();
            chunk.status_code = if i == 0 { status } else { 0 };
            self.send(chunk).await;
        }
    }

    async fn fail(&self, status: i32, error: String) {
        self.send(InferenceChunk {
            done: true,
            status_code: status,
            error,
            ..Default::default()
        })
        .await;
    }

    /// Says "still working" whenever the request has sent nothing for
    /// `every`. The coordinator asked for this; it is how it tells a model
    /// that is thinking from a machine that has gone away. Stops when the
    /// returned guard is dropped, which is when the request ends, however it
    /// ends.
    fn keep_alive(&self, every: Duration) -> KeepAlive {
        let sink = self.clone();
        KeepAlive(tokio::spawn(async move {
            loop {
                let idle = sink.last.lock().unwrap().elapsed();
                if idle < every {
                    tokio::time::sleep(every - idle).await;
                    continue;
                }
                sink.send(InferenceChunk {
                    keepalive: true,
                    ..Default::default()
                })
                .await;
            }
        }))
    }
}

struct KeepAlive(tokio::task::JoinHandle<()>);

impl Drop for KeepAlive {
    fn drop(&mut self) {
        self.0.abort();
    }
}

/// Splits an answer into pieces of at most PIECE_BYTES. An empty answer is
/// one empty piece, so there is always a piece to carry the final fields.
fn split(data: &[u8]) -> Vec<&[u8]> {
    if data.is_empty() {
        return vec![data];
    }
    data.chunks(PIECE_BYTES).collect()
}

/// The paths of the runtime a request may be sent to. The coordinator checks
/// this too; the agent does not take its word for what reaches the runtime,
/// which also answers to paths that download and delete models.
pub fn allowed_path(path: &str) -> bool {
    matches!(
        path,
        "/v1/chat/completions" | "/v1/completions" | "/v1/embeddings"
    )
}

/// Runs one request to completion, sending chunks as they arrive.
pub async fn run(
    runtime: Runtime,
    req: InferenceRequest,
    runtime_model: String,
    model_name: String,
    tx: Sender<AgentMessage>,
) {
    let sink = Sink::new(tx, &req.request_id);
    let _keep_alive = (req.keepalive_seconds > 0)
        .then(|| sink.keep_alive(Duration::from_secs(req.keepalive_seconds as u64)));

    let mut body: Value = match serde_json::from_slice(&req.body) {
        Ok(v) => v,
        Err(e) => {
            let msg = serde_json::json!({"error": {
                "message": format!("invalid JSON body: {e}"),
                "type": "invalid_request_error",
            }});
            sink.send(InferenceChunk {
                data: msg.to_string().into_bytes(),
                done: true,
                status_code: 400,
                ..Default::default()
            })
            .await;
            return;
        }
    };
    let client_wants_usage = body["stream_options"]["include_usage"]
        .as_bool()
        .unwrap_or(false);
    body["model"] = Value::String(runtime_model.clone());
    if req.stream {
        body["stream"] = Value::Bool(true);
        body["stream_options"] = serde_json::json!({ "include_usage": true });
    }
    let prompt_estimate = estimate_prompt_tokens(&body);
    let timeout = Duration::from_secs(req.timeout_seconds.clamp(1, 3600) as u64);

    let resp = match runtime
        .request(
            &req.path,
            serde_json::to_vec(&body).unwrap_or_default(),
            timeout,
        )
        // The id the request has carried since the platform's front door, so
        // one search finds it in the runtime's log too.
        .header("X-Request-ID", &req.request_id)
        .send()
        .await
    {
        Ok(r) => r,
        Err(e) => {
            sink.fail(502, format!("runtime unreachable: {e}")).await;
            return;
        }
    };

    let status = resp.status().as_u16() as i32;
    if status >= 500 {
        let text = resp.text().await.unwrap_or_default();
        sink.fail(502, format!("runtime error {status}: {text}"))
            .await;
        return;
    }
    if status >= 400 {
        // A client error is the customer's to see, not a reason to fail over.
        let data = resp.bytes().await.map(|b| b.to_vec()).unwrap_or_default();
        sink.send_data(
            &data,
            status,
            InferenceChunk {
                done: true,
                ..Default::default()
            },
        )
        .await;
        return;
    }

    if !req.stream {
        let bytes = match resp.bytes().await {
            Ok(b) if b.len() <= MAX_RESPONSE_BYTES => b,
            Ok(_) => {
                sink.fail(502, "runtime response too large".into()).await;
                return;
            }
            Err(e) => {
                sink.fail(502, format!("reading runtime response: {e}"))
                    .await;
                return;
            }
        };
        let (data, usage) = match serde_json::from_slice::<Value>(&bytes) {
            Ok(mut v) => {
                let u = Usage {
                    prompt: v["usage"]["prompt_tokens"]
                        .as_i64()
                        .unwrap_or(prompt_estimate as i64) as i32,
                    completion: v["usage"]["completion_tokens"].as_i64().unwrap_or(0) as i32,
                    exact: v["usage"].is_object(),
                };
                if v.get("model").is_some() {
                    v["model"] = Value::String(model_name.clone());
                }
                (v.to_string().into_bytes(), u)
            }
            Err(_) => (bytes.to_vec(), Usage::default()),
        };
        sink.send_data(
            &data,
            status,
            InferenceChunk {
                done: true,
                prompt_tokens: usage.prompt,
                completion_tokens: usage.completion,
                ..Default::default()
            },
        )
        .await;
        return;
    }

    let mut stream = resp.bytes_stream();
    let mut buf: Vec<u8> = Vec::new();
    let mut usage = Usage::default();
    let mut content_chunks = 0;
    let mut first = true;
    let mut failure: Option<String> = None;

    while let Some(item) = stream.next().await {
        match item {
            Ok(bytes) => buf.extend(bytes.iter().filter(|b| **b != b'\r')),
            Err(e) => {
                failure = Some(format!("stream interrupted: {e}"));
                break;
            }
        }
        while let Some(end) = event_end(&buf) {
            let event: Vec<u8> = buf.drain(..end).collect();
            let out = rewrite_event(
                &String::from_utf8_lossy(&event),
                &model_name,
                client_wants_usage,
                &mut usage,
                &mut content_chunks,
            );
            if !out.is_empty() {
                sink.send_data(
                    out.as_bytes(),
                    if first { status } else { 0 },
                    InferenceChunk::default(),
                )
                .await;
                first = false;
            }
        }
        if buf.len() > MAX_RESPONSE_BYTES {
            failure = Some("the runtime sent an event too large to pass on".into());
            break;
        }
    }

    if !usage.exact {
        // Each content delta is one token for the runtimes we drive.
        usage.prompt = prompt_estimate;
        usage.completion = content_chunks;
    }
    sink.send(InferenceChunk {
        done: true,
        status_code: if first { status } else { 0 },
        error: failure.unwrap_or_default(),
        prompt_tokens: usage.prompt,
        completion_tokens: usage.completion,
        ..Default::default()
    })
    .await;
}

#[cfg(test)]
mod tests {
    use super::*;

    const CHUNK: &str = r#"data: {"id":"c1","object":"chat.completion.chunk","model":"qwen2.5:7b","choices":[{"index":0,"delta":{"content":"Hi"}}]}"#;
    const USAGE: &str = r#"data: {"id":"c1","object":"chat.completion.chunk","model":"qwen2.5:7b","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}"#;

    #[test]
    fn rewrites_model_and_counts_content() {
        let mut u = Usage::default();
        let mut n = 0;
        let out = rewrite_event(
            &format!("{CHUNK}\n\n"),
            "qwen2.5-7b-instruct",
            false,
            &mut u,
            &mut n,
        );
        assert!(out.contains(r#""model":"qwen2.5-7b-instruct""#), "{out}");
        assert!(!out.contains("qwen2.5:7b"));
        assert!(out.ends_with("\n\n"));
        assert_eq!(n, 1);
        assert!(!u.exact);
    }

    #[test]
    fn captures_usage_and_hides_it_unless_requested() {
        let mut u = Usage::default();
        let mut n = 0;
        let hidden = rewrite_event(&format!("{USAGE}\n\n"), "m", false, &mut u, &mut n);
        assert_eq!(
            hidden, "",
            "usage-only chunk must not reach a client that did not ask for it"
        );
        assert_eq!(
            u,
            Usage {
                prompt: 12,
                completion: 7,
                exact: true
            }
        );

        let mut u2 = Usage::default();
        let shown = rewrite_event(&format!("{USAGE}\n\n"), "m", true, &mut u2, &mut n);
        assert!(shown.contains(r#""prompt_tokens":12"#));
        assert!(u2.exact);
    }

    #[test]
    fn passes_done_sentinel_and_ignores_comments() {
        let mut u = Usage::default();
        let mut n = 0;
        let out = rewrite_event(": keep-alive\ndata: [DONE]\n\n", "m", false, &mut u, &mut n);
        assert_eq!(out, "data: [DONE]\n\n");
    }

    #[test]
    fn finds_event_boundaries() {
        assert_eq!(event_end(b"data: a\n\ndata: b"), Some(9));
        assert_eq!(event_end(b"data: partial"), None);
    }

    #[test]
    fn only_the_inference_paths_reach_the_runtime() {
        for p in ["/v1/chat/completions", "/v1/completions", "/v1/embeddings"] {
            assert!(allowed_path(p), "{p}");
        }
        for p in [
            "/api/pull",
            "/api/delete",
            "/v1/models",
            "/v1/chat/completions/../../api/delete",
            "",
        ] {
            assert!(!allowed_path(p), "{p} must not reach the runtime");
        }
    }

    #[test]
    fn a_large_answer_is_split_and_nothing_is_lost() {
        let data: Vec<u8> = (0..PIECE_BYTES * 2 + 17).map(|i| (i % 251) as u8).collect();
        let pieces = split(&data);
        assert_eq!(pieces.len(), 3);
        assert!(pieces.iter().all(|p| p.len() <= PIECE_BYTES));
        assert_eq!(pieces.concat(), data);
        // Even an empty answer has a piece to carry "done".
        assert_eq!(split(&[]).len(), 1);
    }

    fn chunk_of(msg: AgentMessage) -> InferenceChunk {
        match msg.payload {
            Some(agent_message::Payload::InferenceChunk(c)) => c,
            other => panic!("unexpected message {other:?}"),
        }
    }

    #[tokio::test]
    async fn pieces_carry_the_status_first_and_the_ending_last() {
        let (tx, mut rx) = tokio::sync::mpsc::channel(16);
        let sink = Sink::new(tx, "req-1");
        let data = vec![7u8; PIECE_BYTES + 1];
        sink.send_data(
            &data,
            200,
            InferenceChunk {
                done: true,
                completion_tokens: 9,
                ..Default::default()
            },
        )
        .await;
        let a = chunk_of(rx.recv().await.unwrap());
        let b = chunk_of(rx.recv().await.unwrap());
        assert_eq!(
            (a.request_id.as_str(), a.status_code, a.done),
            ("req-1", 200, false)
        );
        assert_eq!(
            a.completion_tokens, 0,
            "usage belongs to the last piece only"
        );
        assert_eq!((b.status_code, b.done, b.completion_tokens), (0, true, 9));
        assert_eq!(a.data.len() + b.data.len(), data.len());
    }

    #[tokio::test(start_paused = true)]
    async fn keep_alives_are_sent_only_while_a_request_is_silent() {
        let (tx, mut rx) = tokio::sync::mpsc::channel(16);
        let sink = Sink::new(tx, "req-1");
        let guard = sink.keep_alive(Duration::from_secs(10));

        // Nothing sent for 25 s: two keep-alives.
        tokio::time::sleep(Duration::from_secs(25)).await;
        let a = chunk_of(rx.try_recv().expect("first keep-alive"));
        let b = chunk_of(rx.try_recv().expect("second keep-alive"));
        assert!(a.keepalive && b.keepalive && !a.done && a.data.is_empty());
        assert_eq!(a.request_id, "req-1");
        assert!(rx.try_recv().is_err());

        // Tokens are flowing: nothing extra is said.
        for _ in 0..5 {
            tokio::time::sleep(Duration::from_secs(4)).await;
            sink.send(InferenceChunk {
                data: b"data: x\n\n".to_vec(),
                ..Default::default()
            })
            .await;
        }
        let mut sent = 0;
        while let Ok(m) = rx.try_recv() {
            assert!(
                !chunk_of(m).keepalive,
                "a keep-alive was sent while tokens were flowing"
            );
            sent += 1;
        }
        assert_eq!(sent, 5);

        // When the request ends, so does the keep-alive.
        drop(guard);
        tokio::time::sleep(Duration::from_secs(60)).await;
        assert!(rx.try_recv().is_err(), "a keep-alive outlived its request");
    }

    #[test]
    fn estimates_prompt_tokens() {
        let body: Value = serde_json::json!({"messages":[{"role":"user","content":"12345678"}]});
        assert_eq!(estimate_prompt_tokens(&body), 2);
        assert_eq!(
            estimate_prompt_tokens(&serde_json::json!({"prompt":"abc"})),
            1
        );
    }
}

/// Runs against a real local Ollama with gemma2:2b pulled. Opt in with:
///   cargo test -- --ignored live_ollama
#[cfg(test)]
mod live {
    use super::*;
    use crate::runtime::{Kind, Runtime};

    async fn collect(stream: bool, client_usage: bool) -> (String, InferenceChunk) {
        let rt = Runtime::new(Kind::Ollama, Kind::Ollama.default_url()).unwrap();
        let mut body = serde_json::json!({
            "model": "gemma-2-2b-it",
            "messages": [{"role": "user", "content": "Reply with exactly: ok"}],
            "max_tokens": 5,
            "stream": stream
        });
        if client_usage {
            body["stream_options"] = serde_json::json!({"include_usage": true});
        }
        let req = InferenceRequest {
            request_id: "r1".into(),
            replica_id: "rep".into(),
            path: "/v1/chat/completions".into(),
            body: serde_json::to_vec(&body).unwrap(),
            stream,
            timeout_seconds: 120,
            keepalive_seconds: 0,
        };
        let (tx, mut rx) = tokio::sync::mpsc::channel(512);
        tokio::spawn(run(rt, req, "gemma2:2b".into(), "gemma-2-2b-it".into(), tx));
        let mut data = String::new();
        loop {
            let msg = rx.recv().await.expect("stream ended without a done chunk");
            let Some(crate::proto::agent_message::Payload::InferenceChunk(c)) = msg.payload else {
                continue;
            };
            data.push_str(&String::from_utf8_lossy(&c.data));
            if c.done {
                return (data, c);
            }
        }
    }

    #[tokio::test]
    #[ignore]
    async fn live_ollama_streaming_reports_exact_usage() {
        let (data, done) = collect(true, false).await;
        assert!(done.error.is_empty(), "error: {}", done.error);
        assert!(
            data.contains(r#""model":"gemma-2-2b-it""#),
            "model not rewritten: {data}"
        );
        assert!(!data.contains("gemma2:2b"), "runtime id leaked: {data}");
        assert!(
            data.trim_end().ends_with("data: [DONE]"),
            "missing [DONE]: {data}"
        );
        assert!(
            !data.contains(r#""usage""#),
            "usage chunk leaked to a client that did not ask: {data}"
        );
        assert!(
            done.prompt_tokens > 0 && done.completion_tokens > 0,
            "no usage: {done:?}"
        );
    }

    #[tokio::test]
    #[ignore]
    async fn live_ollama_streaming_passes_usage_when_requested() {
        let (data, done) = collect(true, true).await;
        assert!(
            data.contains(r#""usage""#),
            "client asked for usage: {data}"
        );
        assert!(done.completion_tokens > 0);
    }

    #[tokio::test]
    #[ignore]
    async fn live_ollama_non_streaming() {
        let (data, done) = collect(false, false).await;
        let v: Value = serde_json::from_str(&data).expect("JSON body");
        assert_eq!(v["model"], "gemma-2-2b-it");
        assert!(v["choices"][0]["message"]["content"].is_string());
        assert!(done.prompt_tokens > 0 && done.completion_tokens > 0);
    }
}
