use serde::{Deserialize, Serialize};

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
            vec![
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
            ]
        } else {
            // Real NVML GPU discovery will run on Linux nodes with NVIDIA drivers.
            // On non-CUDA development hosts, return empty if not in fake GPU mode.
            Vec::new()
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_fake_gpu_detection() {
        let detector = GpuDetector::new(true);
        let gpus = detector.detect();
        assert_eq!(gpus.len(), 2);
        assert_eq!(gpus[0].model, "NVIDIA GeForce RTX 4090");
        assert_eq!(gpus[0].vram_gb, 24);
        assert_eq!(gpus[1].model, "NVIDIA GeForce RTX 5090");
        assert_eq!(gpus[1].vram_gb, 32);
    }
}
