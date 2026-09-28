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
    pub async fn pull(&self, model: &str, mut progress: impl FnMut(String)) -> Result<()> {
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
        while let Some(chunk) = stream.next().await {
            buf.extend_from_slice(&chunk?);
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

    #[test]
    fn parses_runtime_kinds() {
        assert_eq!(Kind::parse("Ollama").unwrap(), Kind::Ollama);
        assert_eq!(Kind::parse("vllm").unwrap(), Kind::Vllm);
        assert!(Kind::parse("tgi").is_err());
        assert_eq!(Kind::Ollama.default_url(), "http://127.0.0.1:11434");
    }
}
