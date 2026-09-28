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
    fake_gpu: bool,
    coordinator_url: String,
    hw_fingerprint: String,
}

impl BenchmarkSuite {
    pub fn new(fake_gpu: bool, coordinator_url: &str, hw_fingerprint: &str) -> Self {
        Self {
            fake_gpu,
            coordinator_url: coordinator_url.to_string(),
            hw_fingerprint: hw_fingerprint.to_string(),
        }
    }

    pub fn run_all(&self) -> BenchmarkReport {
        info!("starting AyeusANN hardware benchmark suite");
        let start = Instant::now();

        let (score_compute, mem_bw) = if self.fake_gpu {
            (85.5, 950.0)
        } else {
            // In physical hardware mode: measure host memory bandwidth.
            // GPU compute is marked 0.0 (unmeasured) unless native GPU compute runner is executed.
            let bw = self.benchmark_memory_bandwidth();
            (0.0, bw)
        };

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
        let temp_path =
            std::env::temp_dir().join(format!("ayeusann_disk_bench_{}.tmp", std::process::id()));
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
    fn benchmark_network_latency(&self) -> f32 {
        let clean_url = self
            .coordinator_url
            .trim_start_matches("http://")
            .trim_start_matches("https://");
        let host_port = clean_url.split('/').next().unwrap_or("127.0.0.1:50051");

        let start = Instant::now();
        if let Ok(addr) = host_port.parse() {
            if let Ok(stream) =
                std::net::TcpStream::connect_timeout(&addr, std::time::Duration::from_millis(500))
            {
                let _ = stream.set_nodelay(true);
                return start.elapsed().as_secs_f32() * 1000.0;
            }
        }

        0.0 // Return 0.0 indicating coordinator connection could not be established
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
        assert_eq!(report.score_compute, 85.5);
        assert_eq!(report.vram_bw_gbps, 950.0);
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
}
