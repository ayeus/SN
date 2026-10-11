//! Local model runtime the agent supervises.
//!
//! Both supported runtimes speak the OpenAI HTTP API on localhost, so serving is
//! identical; they differ only in how a model gets onto the GPU:
//!   * Ollama — pulls 4-bit builds on demand. Used on personal machines (T3),
//!     Apple Silicon, and in development.
//!   * vLLM   — serves full-precision weights on NVIDIA T1/T2 nodes. A vLLM
//!     server serves the model it was started with, so the agent verifies the
//!     model is present rather than pulling it.

use anyhow::{anyhow, bail, Context, Result};
use futures_util::StreamExt;
use serde_json::{json, Value};
use std::time::{Duration, Instant};

/// How long a download may go without any progress before it is abandoned.
const DOWNLOAD_STALL: Duration = Duration::from_secs(300);

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kind {
    Ollama,
    Vllm,
}

impl Kind {
    pub fn parse(s: &str) -> Result<Kind> {
        match s.to_ascii_lowercase().as_str() {
            "ollama" => Ok(Kind::Ollama),
            "vllm" => Ok(Kind::Vllm),
            other => bail!("unknown runtime {other:?}; use ollama or vllm"),
        }
    }

    pub fn name(self) -> &'static str {
        match self {
            Kind::Ollama => "ollama",
            Kind::Vllm => "vllm",
        }
    }

    pub fn default_url(self) -> &'static str {
        match self {
            Kind::Ollama => "http://127.0.0.1:11434",
            Kind::Vllm => "http://127.0.0.1:8000",
        }
    }
}

#[derive(Clone)]
pub struct Runtime {
    pub kind: Kind,
    base: String,
    http: reqwest::Client,
}

impl Runtime {
    pub fn new(kind: Kind, base: &str) -> Result<Self> {
        let http = reqwest::Client::builder()
            .connect_timeout(Duration::from_secs(5))
            .build()
            .context("building HTTP client")?;
        Ok(Self {
            kind,
            base: base.trim_end_matches('/').to_string(),
            http,
        })
    }

    pub fn base_url(&self) -> &str {
        &self.base
    }

    /// Cheap liveness probe, run on every heartbeat.
    pub async fn healthy(&self) -> bool {
        let url = match self.kind {
            Kind::Ollama => format!("{}/api/version", self.base),
            Kind::Vllm => format!("{}/v1/models", self.base),
        };
        self.http
            .get(url)
            .timeout(Duration::from_secs(3))
            .send()
            .await
            .map(|r| r.status().is_success())
            .unwrap_or(false)
    }

    /// Models already present locally; reported so the scheduler can prefer a
    /// warm cache (SRS FR-12, score component "cache-warmth").
    pub async fn cached_models(&self) -> Result<Vec<String>> {
        let (url, list, key) = match self.kind {
            Kind::Ollama => (format!("{}/api/tags", self.base), "models", "name"),
            Kind::Vllm => (format!("{}/v1/models", self.base), "data", "id"),
        };
        let v: Value = self
            .http
            .get(url)
            .timeout(Duration::from_secs(5))
            .send()
            .await?
            .error_for_status()?
            .json()
            .await?;
        Ok(v[list]
            .as_array()
            .map(|a| {
                a.iter()
                    .filter_map(|m| m[key].as_str().map(str::to_string))
                    .collect()
            })
            .unwrap_or_default())
    }

    /// Makes the model available locally, reporting progress.
    pub async fn pull(&self, model: &str, progress: impl FnMut(String)) -> Result<()> {
        self.pull_within(model, progress, DOWNLOAD_STALL).await
    }

    /// `pull`, giving up when the download makes no progress for `stall`.
    pub async fn pull_within(
        &self,
        model: &str,
        mut progress: impl FnMut(String),
        stall: Duration,
    ) -> Result<()> {
        if self.kind == Kind::Vllm {
            if self.cached_models().await?.iter().any(|m| m == model) {
                return Ok(());
            }
            bail!(
                "the vLLM server at {} is not serving {model}; start it with `vllm serve {model}`",
                self.base
            );
        }

        let resp = self
            .http
            .post(format!("{}/api/pull", self.base))
            .json(&json!({ "model": model, "stream": true }))
            .send()
            .await
            .context("runtime unreachable")?;
        if !resp.status().is_success() {
            bail!(
                "pull failed ({}): {}",
                resp.status(),
                resp.text().await.unwrap_or_default()
            );
        }

        let mut stream = resp.bytes_stream();
        let mut buf: Vec<u8> = Vec::new();
        let mut last = Instant::now() - Duration::from_secs(10);
        // A download that has stopped moving is abandoned: the runtime keeps
        // such a connection open and keeps reporting the same figures, and
        // the replica would sit at "downloading" until somebody noticed.
        // Progress is any change in what the runtime reports.
        let mut moved = Instant::now();
        let mut seen = String::new();
        let stalled = |since: Instant| {
            anyhow!(
                "the download of {model} stalled: no progress for {} minutes",
                since.elapsed().as_secs().max(60) / 60
            )
        };
        loop {
            let left = stall.saturating_sub(moved.elapsed());
            let chunk = match tokio::time::timeout(left, stream.next()).await {
                Err(_) => return Err(stalled(moved)),
                Ok(None) => break,
                Ok(Some(chunk)) => chunk?,
            };
            buf.extend_from_slice(&chunk);
            while let Some(pos) = buf.iter().position(|b| *b == b'\n') {
                let line: Vec<u8> = buf.drain(..=pos).collect();
                let Ok(v) = serde_json::from_slice::<Value>(&line) else {
                    continue;
                };
                if let Some(err) = v["error"].as_str() {
                    bail!("pull failed: {err}");
                }
                let status = v["status"].as_str().unwrap_or_default();
                if status == "success" {
                    return Ok(());
                }
                let now = format!("{status}:{}", v["completed"]);
                if now != seen {
                    seen = now;
                    moved = Instant::now();
                } else if moved.elapsed() >= stall {
                    return Err(stalled(moved));
                }
                if last.elapsed() >= Duration::from_secs(2) {
                    last = Instant::now();
                    match (v["completed"].as_f64(), v["total"].as_f64()) {
                        (Some(done), Some(total)) if total > 0.0 => progress(format!(
                            "downloading {:.2} / {:.2} GB ({:.0}%)",
                            done / 1e9,
                            total / 1e9,
                            done / total * 100.0
                        )),
                        _ if !status.is_empty() => progress(status.to_string()),
                        _ => {}
                    }
                }
            }
        }
        bail!("pull stream ended before completion")
    }

    /// The models the runtime has in memory right now. Empty when it cannot
    /// say (vLLM, or a runtime that is not answering).
    pub async fn loaded_models(&self) -> Vec<String> {
        if self.kind == Kind::Vllm {
            return Vec::new();
        }
        let Ok(resp) = self
            .http
            .get(format!("{}/api/ps", self.base))
            .timeout(Duration::from_secs(5))
            .send()
            .await
        else {
            return Vec::new();
        };
        resp.json::<Value>()
            .await
            .ok()
            .and_then(|v| {
                v["models"].as_array().map(|a| {
                    a.iter()
                        .filter_map(|m| m["name"].as_str().map(str::to_string))
                        .collect()
                })
            })
            .unwrap_or_default()
    }

    /// Removes a downloaded model from the runtime's disk.
    pub async fn delete(&self, model: &str) -> Result<()> {
        if self.kind == Kind::Vllm {
            return Ok(());
        }
        let resp = self
            .http
            .delete(format!("{}/api/delete", self.base))
            .json(&json!({ "model": model }))
            .timeout(Duration::from_secs(30))
            .send()
            .await
            .context("runtime unreachable")?;
        if !resp.status().is_success() && resp.status().as_u16() != 404 {
            bail!("delete failed ({})", resp.status());
        }
        Ok(())
    }

    /// Loads weights into memory and pins them there.
    pub async fn load(&self, model: &str) -> Result<()> {
        if self.kind == Kind::Vllm {
            return Ok(()); // vLLM loads at server start
        }
        self.keep_alive(model, -1, Duration::from_secs(600)).await
    }

    /// Keeps a model resident. Ollama evicts idle models after five minutes by
    /// default, which would turn every quiet period into a cold start.
    pub async fn pin(&self, model: &str) -> Result<()> {
        if self.kind == Kind::Ollama {
            self.keep_alive(model, -1, Duration::from_secs(30)).await?;
        }
        Ok(())
    }

    /// Whether the model can answer right now, making sure it stays loaded.
    /// Asked before the agent tells the platform it is still serving a
    /// replica: the runtime may have been restarted, or have evicted the
    /// model, while the connection was down.
    pub async fn still_serving(&self, model: &str) -> bool {
        match self.kind {
            // Loads the model if it is not loaded and pins it either way, so a
            // yes here is true by the time it is reported.
            Kind::Ollama => self.pin(model).await.is_ok(),
            Kind::Vllm => self
                .cached_models()
                .await
                .map(|m| m.iter().any(|x| x == model))
                .unwrap_or(false),
        }
    }

    /// Frees the model's memory once no replica uses it.
    pub async fn unload(&self, model: &str) -> Result<()> {
        if self.kind == Kind::Ollama {
            self.keep_alive(model, 0, Duration::from_secs(30)).await?;
        }
        Ok(())
    }

    async fn keep_alive(&self, model: &str, seconds: i64, timeout: Duration) -> Result<()> {
        let resp = self
            .http
            .post(format!("{}/api/generate", self.base))
            .json(&json!({ "model": model, "keep_alive": seconds }))
            .timeout(timeout)
            .send()
            .await
            .context("runtime unreachable")?;
        if !resp.status().is_success() {
            bail!(
                "runtime refused to load {model} ({}): {}",
                resp.status(),
                resp.text().await.unwrap_or_default()
            );
        }
        Ok(())
    }

    /// One-token generation proving the model answers before it takes traffic.
    pub async fn warm(&self, model: &str) -> Result<()> {
        let resp = self
            .http
            .post(format!("{}/v1/chat/completions", self.base))
            .json(&json!({
                "model": model,
                "messages": [{ "role": "user", "content": "ping" }],
                "max_tokens": 1,
                "stream": false
            }))
            .timeout(Duration::from_secs(300))
            .send()
            .await
            .context("runtime unreachable")?;
        let status = resp.status();
        if !status.is_success() {
            return Err(anyhow!(
                "warm-up failed ({status}): {}",
                resp.text().await.unwrap_or_default()
            ));
        }
        Ok(())
    }

    /// Starts an OpenAI-compatible request against the runtime.
    pub fn request(&self, path: &str, body: Vec<u8>, timeout: Duration) -> reqwest::RequestBuilder {
        self.http
            .post(format!("{}{}", self.base, path))
            .header("Content-Type", "application/json")
            .body(body)
            .timeout(timeout)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A stand-in for `/api/pull` that sends `lines`, one every 50 ms, and
    /// then holds the connection open without another word.
    async fn pull_server(lines: Vec<&'static str>) -> Runtime {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            loop {
                let Ok((mut sock, _)) = listener.accept().await else {
                    return;
                };
                let lines = lines.clone();
                tokio::spawn(async move {
                    let mut buf = [0u8; 4096];
                    let _ = sock.read(&mut buf).await;
                    let _ = sock
                        .write_all(b"HTTP/1.1 200 OK\r\ntransfer-encoding: chunked\r\n\r\n")
                        .await;
                    for l in lines {
                        let body = format!("{l}\n");
                        let _ = sock
                            .write_all(format!("{:x}\r\n{body}\r\n", body.len()).as_bytes())
                            .await;
                        tokio::time::sleep(Duration::from_millis(50)).await;
                    }
                    tokio::time::sleep(Duration::from_secs(3600)).await;
                });
            }
        });
        Runtime::new(Kind::Ollama, &format!("http://{addr}")).unwrap()
    }

    #[tokio::test]
    async fn a_download_that_goes_silent_is_abandoned() {
        let rt = pull_server(vec![
            r#"{"status":"pulling abc","completed":100,"total":1000}"#,
        ])
        .await;
        let started = Instant::now();
        let err = rt
            .pull_within("m:1", |_| {}, Duration::from_millis(400))
            .await
            .expect_err("a download that stopped must not wait for ever");
        assert!(err.to_string().contains("stalled"), "{err}");
        assert!(started.elapsed() < Duration::from_secs(5));
    }

    #[tokio::test]
    async fn a_download_that_keeps_reporting_the_same_figures_is_abandoned() {
        // The runtime repeats itself for as long as the connection is open.
        let rt = pull_server(vec![
            r#"{"status":"pulling abc","completed":100,"total":1000}"#;
            40
        ])
        .await;
        let err = rt
            .pull_within("m:1", |_| {}, Duration::from_millis(400))
            .await
            .expect_err("repeating the same progress is not progress");
        assert!(err.to_string().contains("stalled"), "{err}");
    }

    #[tokio::test]
    async fn a_slow_download_that_is_moving_is_left_alone() {
        let rt = pull_server(vec![
            r#"{"status":"pulling abc","completed":100,"total":1000}"#,
            r#"{"status":"pulling abc","completed":400,"total":1000}"#,
            r#"{"status":"pulling abc","completed":700,"total":1000}"#,
            r#"{"status":"verifying sha256 digest"}"#,
            r#"{"status":"success"}"#,
        ])
        .await;
        rt.pull_within("m:1", |_| {}, Duration::from_millis(400))
            .await
            .expect("each line moved the download on");
    }

    #[test]
    fn parses_runtime_kinds() {
        assert_eq!(Kind::parse("Ollama").unwrap(), Kind::Ollama);
        assert_eq!(Kind::parse("vllm").unwrap(), Kind::Vllm);
        assert!(Kind::parse("tgi").is_err());
        assert_eq!(Kind::Ollama.default_url(), "http://127.0.0.1:11434");
    }
}
