//! Host and GPU telemetry for heartbeats.

use crate::proto::GpuStatus;
use std::process::Command;

/// Reads live NVIDIA GPU utilisation, memory, temperature and power. Other
/// GPUs (Apple Silicon) report nothing rather than invented numbers.
pub fn nvidia_status() -> Vec<GpuStatus> {
    let Ok(out) = Command::new("nvidia-smi")
        .args([
            "--query-gpu=uuid,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw",
            "--format=csv,noheader,nounits",
        ])
        .output()
    else {
        return Vec::new();
    };
    if !out.status.success() {
        return Vec::new();
    }
    parse_nvidia_status(&String::from_utf8_lossy(&out.stdout))
}

pub fn parse_nvidia_status(csv: &str) -> Vec<GpuStatus> {
    csv.lines()
        .filter_map(|line| {
            let p: Vec<&str> = line.split(',').map(str::trim).collect();
            if p.len() < 6 {
                return None;
            }
            let num = |s: &str| s.parse::<f64>().unwrap_or(0.0);
            Some(GpuStatus {
                gpu_uuid: p[0].to_string(),
                utilization_pct: num(p[1]),
                vram_used_mb: num(p[2]) as i32,
                vram_total_mb: num(p[3]) as i32,
                temperature_c: num(p[4]) as i32,
                power_draw_w: num(p[5]) as i32,
            })
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_nvidia_smi() {
        let s = parse_nvidia_status("GPU-abc, 37, 10240, 24564, 61, 212.50\n");
        assert_eq!(s.len(), 1);
        assert_eq!(s[0].gpu_uuid, "GPU-abc");
        assert_eq!(s[0].utilization_pct, 37.0);
        assert_eq!(s[0].vram_total_mb, 24564);
        assert_eq!(s[0].power_draw_w, 212);
    }
}
