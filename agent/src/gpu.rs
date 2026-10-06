use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::process::Command;
use tracing::{info, warn};

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct GpuInfo {
    pub model: String,
    pub vram_gb: i32,
    pub driver_version: String,
    pub cuda_version: String,
    pub compute_capability_major: i32,
    pub compute_capability_minor: i32,
    pub uuid: String,
    pub fingerprint: String,
}

pub struct GpuDetector {
    fake_gpu: bool,
}

impl GpuDetector {
    pub fn new(fake_gpu: bool) -> Self {
        Self { fake_gpu }
    }

    pub fn detect(&self) -> Vec<GpuInfo> {
        if self.fake_gpu {
            warn!("DEVELOPMENT ONLY: fake GPU mode active (--fake-gpu / SN_FAKE_GPU=true)");
            return vec![GpuInfo {
                model: "NVIDIA GeForce RTX 4090".to_string(),
                vram_gb: 24,
                driver_version: "550.54.14".to_string(),
                cuda_version: "12.4".to_string(),
                compute_capability_major: 8,
                compute_capability_minor: 9,
                uuid: "GPU-fake-4090-00000000-0001".to_string(),
                fingerprint: sha256_hex(b"fake_rtx_4090_fingerprint_01"),
            }];
        }

        // 1. macOS Apple Silicon GPU detection
        #[cfg(target_os = "macos")]
        {
            if let Some(apple_gpu) = Self::detect_apple_silicon() {
                return vec![apple_gpu];
            }
        }

        // 2. Physical NVIDIA GPU detection via nvidia-smi
        let nvidias = Self::detect_nvidia_gpus();
        if !nvidias.is_empty() {
            return nvidias;
        }

        info!("no dedicated or integrated acceleration GPU detected on host system");
        Vec::new()
    }

    /// Detects physical NVIDIA GPUs using nvidia-smi CLI
    fn detect_nvidia_gpus() -> Vec<GpuInfo> {
        let output = match Command::new("nvidia-smi")
            .arg("--query-gpu=name,memory.total,driver_version,gpu_uuid,compute_cap")
            .arg("--format=csv,noheader,nounits")
            .output()
        {
            Ok(o) => o,
            Err(_) => return Vec::new(),
        };

        if !output.status.success() {
            return Vec::new();
        }

        let stdout = String::from_utf8_lossy(&output.stdout);
        Self::parse_nvidia_smi_csv(&stdout)
    }

    /// Parses nvidia-smi CSV output into GpuInfo records
    pub fn parse_nvidia_smi_csv(stdout: &str) -> Vec<GpuInfo> {
        let cuda_version = Self::detect_cuda_version();
        let mut gpus = Vec::new();

        for line in stdout.lines() {
            let parts: Vec<&str> = line.split(',').map(|s| s.trim()).collect();
            if parts.len() >= 4 {
                let model = parts[0].to_string();
                let mem_mb = parts[1].parse::<i32>().unwrap_or(0);
                let vram_gb = (mem_mb + 512) / 1024; // Round to nearest GB
                let driver_version = parts[2].to_string();
                let uuid = parts[3].to_string();

                let mut major = 0;
                let mut minor = 0;
                if parts.len() >= 5 {
                    let cap_str = parts[4];
                    if let Some((maj, min)) = cap_str.split_once('.') {
                        major = maj.parse::<i32>().unwrap_or(0);
                        minor = min.parse::<i32>().unwrap_or(0);
                    } else if let Ok(maj) = cap_str.parse::<i32>() {
                        major = maj;
                    }
                }

                // Identifies the card, not its software: the driver version is
                // reported separately and changes with every update.
                let fp_input = format!("{}:{}", uuid, vram_gb);
                let fingerprint = sha256_hex(fp_input.as_bytes());

                info!(
                    model = %model,
                    vram_gb = vram_gb,
                    uuid = %uuid,
                    compute_cap = format!("{}.{}", major, minor),
                    "detected physical NVIDIA GPU via nvidia-smi"
                );

                gpus.push(GpuInfo {
                    model,
                    vram_gb,
                    driver_version,
                    cuda_version: cuda_version.clone(),
                    compute_capability_major: major,
                    compute_capability_minor: minor,
                    uuid,
                    fingerprint,
                });
            }
        }

        gpus
    }

    /// Detects installed CUDA version from nvidia-smi or nvcc CLI.
    /// Returns empty string if unavailable.
    fn detect_cuda_version() -> String {
        // Try nvcc --version first
        if let Ok(out) = Command::new("nvcc").arg("--version").output() {
            if out.status.success() {
                let s = String::from_utf8_lossy(&out.stdout);
                if let Some(idx) = s.find("release ") {
                    let rest = &s[idx + 8..];
                    let ver = rest.split(',').next().unwrap_or("").trim();
                    if !ver.is_empty() {
                        return ver.to_string();
                    }
                }
            }
        }

        // Fallback to parsing nvidia-smi header
        if let Ok(out) = Command::new("nvidia-smi").output() {
            if out.status.success() {
                let s = String::from_utf8_lossy(&out.stdout);
                if let Some(idx) = s.find("CUDA Version: ") {
                    let rest = &s[idx + 14..];
                    let ver = rest.split_whitespace().next().unwrap_or("").trim();
                    if !ver.is_empty() {
                        return ver.to_string();
                    }
                }
            }
        }

        String::new()
    }

    #[cfg(target_os = "macos")]
    fn detect_apple_silicon() -> Option<GpuInfo> {
        let output = Command::new("system_profiler")
            .arg("SPDisplaysDataType")
            .arg("SPHardwareDataType")
            .output()
            .ok()?;

        if !output.status.success() {
            return None;
        }

        let stdout = String::from_utf8_lossy(&output.stdout);
        Self::parse_macos_profiler(&stdout)
    }

    /// Parses macOS system_profiler displays and hardware output
    #[cfg(any(target_os = "macos", test))]
    pub fn parse_macos_profiler(stdout: &str) -> Option<GpuInfo> {
        let mut chip_name = "Apple Silicon GPU".to_string();
        let mut memory_gb = 0;
        let mut hw_uuid = String::new();
        let mut serial_num = String::new();

        for line in stdout.lines() {
            let trimmed = line.trim();
            if trimmed.starts_with("Chip:") || trimmed.starts_with("Chipset Model:") {
                if let Some((_, val)) = trimmed.split_once(':') {
                    chip_name = format!("Apple {} GPU", val.trim());
                }
            }
            if trimmed.starts_with("Memory:") {
                if let Some((_, val)) = trimmed.split_once(':') {
                    let parts: Vec<&str> = val.split_whitespace().collect();
                    if !parts.is_empty() {
                        if let Ok(gb) = parts[0].parse::<i32>() {
                            memory_gb = gb;
                        }
                    }
                }
            }
            if trimmed.starts_with("Hardware UUID:") {
                if let Some((_, val)) = trimmed.split_once(':') {
                    hw_uuid = val.trim().to_string();
                }
            }
            if trimmed.starts_with("Serial Number (system):") {
                if let Some((_, val)) = trimmed.split_once(':') {
                    serial_num = val.trim().to_string();
                }
            }
        }

        // If system_profiler did not report memory, query sysinfo total memory
        if memory_gb == 0 {
            use sysinfo::System;
            let sys = System::new_all();
            memory_gb = (sys.total_memory() / (1024 * 1024 * 1024)) as i32;
        }

        if chip_name.contains("Apple Apple") {
            chip_name = chip_name.replace("Apple Apple", "Apple");
        }

        // Apple Silicon is only detected if we found an Apple chip or hardware UUID
        if hw_uuid.is_empty() && !chip_name.contains("Apple") {
            return None;
        }

        let device_id = if !hw_uuid.is_empty() {
            format!("apple-soc-{}", hw_uuid.to_lowercase())
        } else {
            "apple-soc-unified".to_string()
        };

        let fp_input = format!("{}:{}:{}", hw_uuid, serial_num, memory_gb);
        let fingerprint = sha256_hex(fp_input.as_bytes());

        info!(
            chip = %chip_name,
            unified_memory_gb = memory_gb,
            id = %device_id,
            "detected physical Apple Silicon with Unified Memory"
        );

        Some(GpuInfo {
            model: chip_name,
            vram_gb: memory_gb,
            driver_version: "Metal (macOS Unified)".to_string(),
            cuda_version: String::new(), // Apple does not have CUDA; explicitly empty
            compute_capability_major: 0,
            compute_capability_minor: 0,
            uuid: device_id,
            fingerprint,
        })
    }
}

/// The per-GPU fingerprint agents up to 0.3 computed, kept only to recognise
/// machines they enrolled (see `legacy_fingerprint` in main.rs). For NVIDIA it
/// included the driver version; Apple's is unchanged.
pub fn legacy_fingerprint(g: &GpuInfo, instance: Option<&str>) -> String {
    if g.uuid.starts_with("apple-soc-") {
        return g.fingerprint.clone();
    }
    // --instance appends to the id after detection; 0.3 hashed it before.
    let suffix = instance.map(|i| format!("-instance-{i}"));
    let uuid = suffix
        .as_deref()
        .and_then(|s| g.uuid.strip_suffix(s))
        .unwrap_or(&g.uuid);
    sha256_hex(format!("{}:{}:{}", uuid, g.driver_version, g.vram_gb).as_bytes())
}

/// Computes a standard hex SHA-256 string prefixed with "sha256:"
pub fn sha256_hex(input: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(input);
    format!("sha256:{}", hex::encode(hasher.finalize()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_fake_gpu_detection() {
        let detector = GpuDetector::new(true);
        let gpus = detector.detect();
        assert_eq!(gpus.len(), 1);
        assert_eq!(gpus[0].model, "NVIDIA GeForce RTX 4090");
        assert_eq!(gpus[0].vram_gb, 24);
        assert!(gpus[0].fingerprint.starts_with("sha256:"));
        assert_eq!(gpus[0].fingerprint.len(), 7 + 64);
    }

    #[test]
    fn test_parse_nvidia_smi_csv() {
        let sample = "NVIDIA A100-SXM4-80GB, 81920, 535.104.05, GPU-12345678-abcd-ef01-2345-6789abcdef01, 8.0\n";
        let gpus = GpuDetector::parse_nvidia_smi_csv(sample);
        assert_eq!(gpus.len(), 1);
        assert_eq!(gpus[0].model, "NVIDIA A100-SXM4-80GB");
        assert_eq!(gpus[0].vram_gb, 80);
        assert_eq!(gpus[0].driver_version, "535.104.05");
        assert_eq!(gpus[0].compute_capability_major, 8);
        assert_eq!(gpus[0].compute_capability_minor, 0);
        assert!(gpus[0].fingerprint.starts_with("sha256:"));
    }

    #[test]
    fn test_parse_macos_profiler() {
        let sample = "Graphics/Displays:\n    Chipset Model: Apple M2 Pro\nHardware Overview:\n    Memory: 32 GB\n    Hardware UUID: 12345678-ABCD-EF01-2345-6789ABCDEF01\n";
        let gpu = GpuDetector::parse_macos_profiler(sample).expect("should parse macos profiler");
        assert_eq!(gpu.model, "Apple M2 Pro GPU");
        assert_eq!(gpu.vram_gb, 32);
        assert_eq!(gpu.cuda_version, ""); // Explicitly no CUDA
        assert_eq!(gpu.compute_capability_major, 0);
        assert!(gpu.fingerprint.starts_with("sha256:"));
    }

    #[test]
    fn test_real_sha256_hex() {
        // SHA-256 of empty string is e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
        let h = sha256_hex(b"");
        assert_eq!(
            h,
            "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
    }
}
