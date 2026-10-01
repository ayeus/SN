//! Executes tunnelled inference requests against the local runtime and streams
//! the answer back over the coordinator session (ADR-011).

use crate::proto::{agent_message, AgentMessage, InferenceChunk, InferenceRequest};
use crate::runtime::Runtime;
use futures_util::StreamExt;
use serde_json::Value;
use std::time::Duration;
use tokio::sync::mpsc::Sender;

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

async fn send(tx: &Sender<AgentMessage>, chunk: InferenceChunk) {
    let _ = tx
        .send(AgentMessage {
            payload: Some(agent_message::Payload::InferenceChunk(chunk)),
        })
        .await;
}

fn error_chunk(id: &str, status: i32, error: String) -> InferenceChunk {
    InferenceChunk {
        request_id: id.to_string(),
        done: true,
        status_code: status,
        error,
        ..Default::default()
    }
}

/// Runs one request to completion, sending chunks as they arrive.
pub async fn run(
    runtime: Runtime,
    req: InferenceRequest,
    runtime_model: String,
    model_name: String,
    tx: Sender<AgentMessage>,
) {
    let id = req.request_id.clone();

    let mut body: Value = match serde_json::from_slice(&req.body) {
        Ok(v) => v,
        Err(e) => {
            let msg = format!(
                r#"{{"error":{{"message":"invalid JSON body: {e}","type":"invalid_request_error"}}}}"#
            );
            send(
                &tx,
                InferenceChunk {
                    request_id: id,
                    data: msg.into_bytes(),
                    done: true,
                    status_code: 400,
                    ..Default::default()
                },
            )
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
    let timeout = Duration::from_secs(req.timeout_seconds.clamp(1, 600) as u64);

    let resp = match runtime
        .request(
            &req.path,
            serde_json::to_vec(&body).unwrap_or_default(),
            timeout,
        )
        .send()
        .await
    {
        Ok(r) => r,
        Err(e) => {
            send(
                &tx,
                error_chunk(&id, 502, format!("runtime unreachable: {e}")),
            )
            .await;
            return;
        }
    };

    let status = resp.status().as_u16() as i32;
    if status >= 500 {
        let text = resp.text().await.unwrap_or_default();
        send(
            &tx,
            error_chunk(&id, 502, format!("runtime error {status}: {text}")),
        )
        .await;
        return;
    }
    if status >= 400 {
        // A client error is the customer's to see, not a reason to fail over.
        let data = resp.bytes().await.map(|b| b.to_vec()).unwrap_or_default();
        send(
            &tx,
            InferenceChunk {
                request_id: id,
                data,
                done: true,
                status_code: status,
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
                send(
                    &tx,
                    error_chunk(&id, 502, "runtime response too large".into()),
                )
                .await;
                return;
            }
            Err(e) => {
                send(
                    &tx,
                    error_chunk(&id, 502, format!("reading runtime response: {e}")),
                )
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
        send(
            &tx,
            InferenceChunk {
                request_id: id,
                data,
                done: true,
                status_code: status,
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
                send(
                    &tx,
                    InferenceChunk {
                        request_id: id.clone(),
                        data: out.into_bytes(),
                        status_code: if first { status } else { 0 },
                        ..Default::default()
                    },
                )
                .await;
                first = false;
            }
        }
    }

    if !usage.exact {
        // Each content delta is one token for the runtimes we drive.
        usage.prompt = prompt_estimate;
        usage.completion = content_chunks;
    }
    send(
        &tx,
        InferenceChunk {
            request_id: id,
            done: true,
            status_code: if first { status } else { 0 },
            error: failure.unwrap_or_default(),
            prompt_tokens: usage.prompt,
            completion_tokens: usage.completion,
            ..Default::default()
        },
    )
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
