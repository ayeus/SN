use serde::{Deserialize, Serialize};
use std::time::Instant;
use tracing::info;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct BenchmarkReport {
    pub score_compute: f32,
    pub vram_bw_gbps: f32,
    pub disk_read_mbps: f32,
    pub disk_write_mbps: f32,
    pub net_up_mbps: f32,
    pub net_down_mbps: f32,
    pub latency_pop_ms: f32,
    pub hw_fingerprint: String,
}

pub struct BenchmarkSuite {
    coordinator_url: String,
    hw_fingerprint: String,
}

impl BenchmarkSuite {
    pub fn new(_fake_gpu: bool, coordinator_url: &str, hw_fingerprint: &str) -> Self {
        Self {
            coordinator_url: coordinator_url.to_string(),
            hw_fingerprint: hw_fingerprint.to_string(),
        }
    }

    pub fn run_all(&self) -> BenchmarkReport {
        info!("starting AyeusANN hardware benchmark suite");
        let start = Instant::now();

        // Measurements are always real, including in --fake-gpu mode: that mode
        // simulates a GPU *identity* for development, never benchmark results.
        // GPU compute is 0.0 ("not measured") until a native compute kernel
        // exists; the control plane stores that as NULL.
        let score_compute = 0.0;
        let mem_bw = self.benchmark_memory_bandwidth();

        let (disk_read, disk_write) = self.benchmark_disk_io();
        let latency_ms = self.benchmark_network_latency();

        let elapsed = start.elapsed();
        info!(
            duration_ms = elapsed.as_millis(),
            score_compute = score_compute,
            vram_bw_gbps = mem_bw,
            disk_read_mbps = disk_read,
            disk_write_mbps = disk_write,
            latency_ms = latency_ms,
            "benchmark suite completed"
        );

        BenchmarkReport {
            score_compute,
            vram_bw_gbps: mem_bw,
            disk_read_mbps: disk_read,
            disk_write_mbps: disk_write,
            net_up_mbps: 0.0,   // Unmeasured: no speedtest payload executed
            net_down_mbps: 0.0, // Unmeasured: no speedtest payload executed
            latency_pop_ms: latency_ms,
            hw_fingerprint: self.hw_fingerprint.clone(),
        }
    }

    /// Real sequential memory bandwidth benchmark (GB/s)
    fn benchmark_memory_bandwidth(&self) -> f32 {
        let size_bytes = 32 * 1024 * 1024; // 32MB test buffer
        let mut buffer = vec![0u8; size_bytes];

        let start = Instant::now();
        // Sequential write
        for (i, byte) in buffer.iter_mut().enumerate() {
            *byte = (i & 0xFF) as u8;
        }
        // Sequential read
        let mut checksum: u64 = 0;
        for byte in buffer.iter() {
            checksum = checksum.wrapping_add(*byte as u64);
        }
        let elapsed = start.elapsed().as_secs_f32();
        if elapsed > 0.0 && checksum > 0 {
            let gb_transferred = (size_bytes as f32 * 2.0) / 1_000_000_000.0;
            gb_transferred / elapsed
        } else {
            0.0
        }
    }

    /// Real disk I/O benchmark measuring write and read speeds (MB/s)
    fn benchmark_disk_io(&self) -> (f32, f32) {
        use std::io::{Read, Write};
        let unique = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        let temp_path = std::env::temp_dir().join(format!(
            "ayeusann_disk_bench_{}_{unique}.tmp",
            std::process::id()
        ));
        let data = vec![0xABu8; 10 * 1024 * 1024]; // 10MB test payload

        // Write benchmark
        let w_start = Instant::now();
        let mut write_success = false;
        if let Ok(mut f) = std::fs::File::create(&temp_path) {
            if f.write_all(&data).is_ok() && f.sync_all().is_ok() {
                write_success = true;
            }
        }
        let w_sec = w_start.elapsed().as_secs_f32();
        let write_mbps = if write_success && w_sec > 0.0 {
            10.0 / w_sec
        } else {
            0.0
        };

        // Read benchmark
        let r_start = Instant::now();
        let mut read_success = false;
        if let Ok(mut f) = std::fs::File::open(&temp_path) {
            let mut buf = Vec::new();
            if f.read_to_end(&mut buf).is_ok() && buf.len() == data.len() {
                read_success = true;
            }
        }
        let r_sec = r_start.elapsed().as_secs_f32();
        let read_mbps = if read_success && r_sec > 0.0 {
            10.0 / r_sec
        } else {
            0.0
        };

        let _ = std::fs::remove_file(&temp_path);
        (read_mbps, write_mbps)
    }

    /// Real network latency test to coordinator TCP port
    /// Median TCP connect time to the coordinator, in milliseconds. The name
    /// is resolved first so DNS time is not counted. Returns 0.0 (stored as
    /// "not measured") when the coordinator cannot be reached.
    fn benchmark_network_latency(&self) -> f32 {
        use std::net::ToSocketAddrs;
        let is_tls = self.coordinator_url.starts_with("https://");
        let clean = self
            .coordinator_url
            .trim_start_matches("http://")
            .trim_start_matches("https://");
        let authority = clean.split('/').next().unwrap_or_default();
        let target = if authority
            .rsplit(':')
            .next()
            .map(|p| p.parse::<u16>().is_ok())
            .unwrap_or(false)
            && authority.contains(':')
        {
            authority.to_string()
        } else {
            format!("{authority}:{}", if is_tls { 443 } else { 80 })
        };
        // A name can resolve to several addresses (localhost → ::1 and
        // 127.0.0.1); use the first one that accepts a connection.
        let timeout = std::time::Duration::from_secs(2);
        let Some(addr) = target.to_socket_addrs().ok().and_then(|mut addrs| {
            addrs.find(|a| std::net::TcpStream::connect_timeout(a, timeout).is_ok())
        }) else {
            return 0.0;
        };

        let mut samples: Vec<f32> = (0..5)
            .filter_map(|_| {
                let start = Instant::now();
                std::net::TcpStream::connect_timeout(&addr, timeout)
                    .ok()
                    .map(|_| start.elapsed().as_secs_f32() * 1000.0)
            })
            .collect();
        if samples.is_empty() {
            return 0.0;
        }
        samples.sort_by(|a, b| a.total_cmp(b));
        samples[samples.len() / 2]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_benchmark_suite_execution() {
        let suite =
            BenchmarkSuite::new(true, "http://127.0.0.1:50051", "sha256:test_hw_fingerprint");
        let report = suite.run_all();
        // Fake-GPU mode must not invent results.
        assert_eq!(report.score_compute, 0.0);
        assert!(report.vram_bw_gbps > 0.0 && report.vram_bw_gbps < 900.0);
        assert_eq!(report.hw_fingerprint, "sha256:test_hw_fingerprint");
    }

    #[test]
    fn test_benchmark_suite_real_hardware() {
        let suite = BenchmarkSuite::new(
            false,
            "http://127.0.0.1:50051",
            "sha256:test_hw_fingerprint",
        );
        let report = suite.run_all();
        // Compute score is honestly 0.0 when no native GPU compute kernel was run
        assert_eq!(report.score_compute, 0.0);
        // Memory bandwidth and disk I/O should be measured positive numbers
        assert!(report.vram_bw_gbps > 0.0);
        assert!(report.disk_read_mbps > 0.0);
        assert!(report.disk_write_mbps > 0.0);
    }

    #[test]
    fn latency_resolves_hostnames() {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        let suite = BenchmarkSuite::new(false, &format!("http://localhost:{port}"), "fp");
        assert!(
            suite.benchmark_network_latency() > 0.0,
            "localhost must resolve"
        );
        let dead = BenchmarkSuite::new(false, "http://127.0.0.1:1", "fp");
        assert_eq!(dead.benchmark_network_latency(), 0.0);
    }
}
