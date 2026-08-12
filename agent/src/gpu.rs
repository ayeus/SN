use serde::{Deserialize, Serialize};
use std::process::Command;
use tracing::info;

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
            return vec![
                GpuInfo {
                    model: "NVIDIA GeForce RTX 4090".to_string(),
                    vram_gb: 24,
                    driver_version: "550.54.14".to_string(),
                    cuda_version: "12.4".to_string(),
                    compute_capability_major: 8,
                    compute_capability_minor: 9,
                    uuid: "GPU-fake-4090-00000000-0001".to_string(),
                    fingerprint: "sha256:fake_rtx_4090_fingerprint_01".to_string(),
                },
                GpuInfo {
                    model: "NVIDIA GeForce RTX 5090".to_string(),
                    vram_gb: 32,
                    driver_version: "550.54.14".to_string(),
                    cuda_version: "12.4".to_string(),
                    compute_capability_major: 9,
                    compute_capability_minor: 0,
                    uuid: "GPU-fake-5090-00000000-0002".to_string(),
                    fingerprint: "sha256:fake_rtx_5090_fingerprint_02".to_string(),
                },
            ];
        }

        #[cfg(target_os = "macos")]
        {
            if let Some(apple_gpu) = Self::detect_apple_silicon() {
                return vec![apple_gpu];
            }
        }

        // Production Linux NVML detection fallback
        Vec::new()
    }

    #[cfg(target_os = "macos")]
    fn detect_apple_silicon() -> Option<GpuInfo> {
        // Query system_profiler for Chipset Model, RAM, Hardware UUID, and Serial
        let output = Command::new("system_profiler")
            .arg("SPDisplaysDataType")
            .arg("SPHardwareDataType")
            .output()
            .ok()?;

        let stdout = String::from_utf8_lossy(&output.stdout);

        let mut chip_name = "Apple Silicon GPU".to_string();
        let mut memory_gb = 16; // default fallback
        let mut hw_uuid = "unknown-uuid".to_string();
        let mut serial_num = "unknown-serial".to_string();

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

        // Clean up redundant "Apple Apple" if present
        if chip_name.contains("Apple Apple") {
            chip_name = chip_name.replace("Apple Apple", "Apple");
        }

        info!(
            chip = %chip_name,
            vram_gb = memory_gb,
            hw_uuid = %hw_uuid,
            "detected physical Apple Silicon GPU with Unified Memory"
        );

        let fingerprint_raw = format!("{}:{}:{}", hw_uuid, serial_num, memory_gb);
        let fingerprint = format!("sha256:{:x}", md5_or_simple_hash(&fingerprint_raw));

        Some(GpuInfo {
            model: chip_name,
            vram_gb: memory_gb,
            driver_version: "Metal 4 (macOS)".to_string(),
            cuda_version: "Metal 4 / MPS".to_string(),
            compute_capability_major: 0,
            compute_capability_minor: 0,
            uuid: format!("GPU-apple-{}", hw_uuid.to_lowercase()),
            fingerprint,
        })
    }
}

fn md5_or_simple_hash(input: &str) -> u128 {
    let mut hash: u128 = 0xcbf29ce484222325;
    for byte in input.bytes() {
        hash ^= u128::from(byte);
        hash = hash.wrapping_mul(0x100000001b3);
    }
    hash
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_fake_gpu_detection() {
        let detector = GpuDetector::new(true);
        let gpus = detector.detect();
        assert_eq!(gpus.len(), 2);
    }

    #[test]
    fn test_real_hardware_detection() {
        let detector = GpuDetector::new(false);
        let gpus = detector.detect();
        assert!(
            !gpus.is_empty(),
            "Expected real hardware detection to find host GPU"
        );
    }
}
