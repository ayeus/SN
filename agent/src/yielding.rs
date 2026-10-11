//! Staying out of the owner's way.
//!
//! The machine belongs to somebody, and lending its GPU must never cost them
//! the use of it. The agent gives the GPU back when the owner needs it: when
//! they say so (`pause`), on battery, while another program is using the GPU,
//! while they are at the keyboard (if they ask for that), and outside the
//! hours they offer. The rules live in a file on the machine, the owner's
//! choice always wins, and the platform is only told the outcome.

use anyhow::{anyhow, bail, Context, Result};
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};
use std::time::Duration;

/// `paused_until` when the owner paused with no end: until they resume.
pub const UNTIL_RESUMED: i64 = i64::MAX;

/// Another program holding this much GPU memory means the GPU is in use.
const OTHER_GPU_MB: i64 = 1024;
/// The GPU this busy, while the agent itself is answering nothing, means
/// somebody else is using it.
const BUSY_UTILISATION_PCT: f64 = 30.0;
/// The owner counts as at the keyboard for this long after their last input.
const ACTIVE_SECS: u64 = 300;

fn yes() -> bool {
    true
}

/// The owner's rules. The defaults suit a laptop lent to a friend: give way
/// on battery and to anything else that wants the GPU, serve at any hour,
/// and do not watch the keyboard unless asked.
#[derive(Serialize, Deserialize, Clone, Debug, PartialEq)]
pub struct Rules {
    #[serde(default = "yes")]
    pub on_battery: bool,
    #[serde(default = "yes")]
    pub when_gpu_busy: bool,
    #[serde(default)]
    pub when_active: bool,
    /// The hours the machine is offered, as "22:00-07:00" in its own local
    /// time. None means any time.
    #[serde(default)]
    pub hours: Option<String>,
    /// Paused by the owner until this moment (Unix seconds), or
    /// UNTIL_RESUMED.
    #[serde(default)]
    pub paused_until: Option<i64>,
}

impl Default for Rules {
    fn default() -> Self {
        Rules {
            on_battery: true,
            when_gpu_busy: true,
            when_active: false,
            hours: None,
            paused_until: None,
        }
    }
}

pub fn path(data_dir: &Path) -> PathBuf {
    data_dir.join("yield.json")
}

pub fn load(data_dir: &Path) -> Rules {
    std::fs::read(path(data_dir))
        .ok()
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default()
}

pub fn save(data_dir: &Path, rules: &Rules) -> Result<()> {
    std::fs::create_dir_all(data_dir)
        .with_context(|| format!("creating {}", data_dir.display()))?;
    let p = path(data_dir);
    let tmp = p.with_extension("json.tmp");
    std::fs::write(&tmp, serde_json::to_vec_pretty(rules)?)?;
    std::fs::rename(&tmp, &p)?;
    Ok(())
}

/// What is true of the machine right now. None means it could not be
/// measured here, and a rule that depends on it does not fire: the agent
/// never yields, or refuses to, on a guess.
#[derive(Default, Debug, Clone)]
pub struct Probe {
    /// Unix seconds.
    pub now: i64,
    /// Minutes since local midnight.
    pub minute_of_day: Option<u32>,
    pub on_battery: Option<bool>,
    /// GPU memory held by programs other than the model runtime.
    pub other_gpu_mb: Option<i64>,
    /// The busiest GPU, in percent.
    pub gpu_utilisation_pct: Option<f64>,
    /// Requests this agent is answering and models it is loading.
    pub own_requests: usize,
    /// Seconds since the owner last touched the keyboard or mouse.
    pub idle_secs: Option<u64>,
}

/// Decides whether the machine should be given back, and says why in words
/// its owner would use. The first rule that applies gives the reason.
pub fn decide(rules: &Rules, p: &Probe) -> Option<String> {
    if let Some(until) = rules.paused_until {
        if until == UNTIL_RESUMED {
            return Some("paused by its owner".into());
        }
        if p.now < until {
            let left = (until - p.now) as u64;
            return Some(format!("paused by its owner for another {}", words(left)));
        }
    }
    if let (Some(hours), Some(minute)) = (rules.hours.as_deref(), p.minute_of_day) {
        if let Some((from, to)) = parse_hours(hours) {
            if !within(from, to, minute) {
                return Some(format!("outside the hours it is offered ({hours})"));
            }
        }
    }
    if rules.on_battery && p.on_battery == Some(true) {
        return Some("on battery".into());
    }
    if rules.when_gpu_busy {
        if p.other_gpu_mb.is_some_and(|mb| mb >= OTHER_GPU_MB) {
            return Some("the GPU is in use by another program".into());
        }
        // Busy while this agent is answering nothing: it is not our work.
        if p.own_requests == 0
            && p.gpu_utilisation_pct
                .is_some_and(|u| u >= BUSY_UTILISATION_PCT)
        {
            return Some("the GPU is in use by another program".into());
        }
    }
    if rules.when_active && p.idle_secs.is_some_and(|s| s < ACTIVE_SECS) {
        return Some("its owner is using the computer".into());
    }
    None
}

/// A length of time for people: "2 hours", "45 minutes".
fn words(secs: u64) -> String {
    let unit = |n: u64, w: &str| format!("{n} {w}{}", if n == 1 { "" } else { "s" });
    if secs >= 36 * 3600 {
        unit(secs.div_ceil(86_400), "day")
    } else if secs >= 90 * 60 {
        unit(secs.div_ceil(3600), "hour")
    } else {
        unit(secs.div_ceil(60).max(1), "minute")
    }
}

/// Reads "22:00-07:00" into minutes since midnight.
pub fn parse_hours(s: &str) -> Option<(u32, u32)> {
    let (from, to) = s.split_once('-')?;
    let minute = |t: &str| {
        let (h, m) = t.trim().split_once(':')?;
        let (h, m): (u32, u32) = (h.parse().ok()?, m.parse().ok()?);
        (h < 24 && m < 60).then_some(h * 60 + m)
    };
    Some((minute(from)?, minute(to)?))
}

/// Whether `minute` falls in the window, which may run past midnight. A
/// window that starts and ends at the same time is the whole day.
pub fn within(from: u32, to: u32, minute: u32) -> bool {
    if from == to {
        true
    } else if from < to {
        (from..to).contains(&minute)
    } else {
        minute >= from || minute < to
    }
}

/// Reads "2h", "30m", "90s" or "1d" (a bare number is minutes).
pub fn parse_duration(s: &str) -> Result<Duration> {
    let s = s.trim();
    let (digits, unit) = match s.char_indices().find(|(_, c)| !c.is_ascii_digit()) {
        Some((i, _)) => s.split_at(i),
        None => (s, "m"),
    };
    let n: u64 = digits
        .parse()
        .map_err(|_| anyhow!("{s:?} is not a length of time; try 2h, 30m or 1d"))?;
    let secs = match unit.trim() {
        "s" | "sec" | "secs" => n,
        "m" | "min" | "mins" => n * 60,
        "h" | "hr" | "hrs" | "hour" | "hours" => n * 3600,
        "d" | "day" | "days" => n * 86_400,
        other => bail!("unknown unit {other:?}; use s, m, h or d"),
    };
    if secs == 0 {
        bail!("the pause must be longer than nothing");
    }
    Ok(Duration::from_secs(secs))
}

// ─── Measuring the machine ──────────────────────────────────

fn output(cmd: &str, args: &[&str]) -> Option<String> {
    let out = std::process::Command::new(cmd).args(args).output().ok()?;
    out.status
        .success()
        .then(|| String::from_utf8_lossy(&out.stdout).to_string())
}

/// `pmset -g batt` on macOS: "Now drawing from 'Battery Power'".
pub fn parse_pmset(s: &str) -> Option<bool> {
    let first = s.lines().next()?;
    if first.contains("Battery Power") {
        Some(true)
    } else if first.contains("AC Power") {
        Some(false)
    } else {
        None
    }
}

/// Whether the machine is running on its battery. None on a machine without
/// one, or where it cannot be told.
pub fn on_battery() -> Option<bool> {
    if cfg!(target_os = "macos") {
        return parse_pmset(&output("pmset", &["-g", "batt"])?);
    }
    if cfg!(target_os = "linux") {
        let mut battery = None;
        for entry in std::fs::read_dir("/sys/class/power_supply").ok()?.flatten() {
            let p = entry.path();
            let read = |f: &str| {
                std::fs::read_to_string(p.join(f))
                    .map(|s| s.trim().to_string())
                    .unwrap_or_default()
            };
            match read("type").as_str() {
                // Plugged in settles it, whatever the battery says.
                "Mains" | "USB" if read("online") == "1" => return Some(false),
                "Battery" => battery = Some(read("status") == "Discharging"),
                _ => {}
            }
        }
        return battery;
    }
    if cfg!(target_os = "windows") {
        // BatteryStatus 1 is "discharging"; a desktop prints nothing.
        let out = output(
            "powershell",
            &[
                "-NoProfile",
                "-Command",
                "(Get-CimInstance Win32_Battery).BatteryStatus",
            ],
        )?;
        let status = out.split_whitespace().next()?;
        return Some(status == "1");
    }
    None
}

/// Sums GPU memory held by programs other than the model runtime, from
/// `nvidia-smi --query-compute-apps=process_name,used_memory`.
pub fn parse_compute_apps(csv: &str, runtime: &str) -> i64 {
    csv.lines()
        .filter_map(|l| {
            let (name, mb) = l.rsplit_once(',')?;
            let name = name.to_ascii_lowercase();
            // vLLM runs as a Python program.
            let ours = name.contains(runtime) || (runtime == "vllm" && name.contains("python"));
            (!ours).then(|| mb.trim().parse::<i64>().unwrap_or(0))
        })
        .sum()
}

fn other_gpu_mb(runtime: &str) -> Option<i64> {
    let out = output(
        "nvidia-smi",
        &[
            "--query-compute-apps=process_name,used_memory",
            "--format=csv,noheader,nounits",
        ],
    )?;
    Some(parse_compute_apps(&out, runtime))
}

/// `ioreg -c IOHIDSystem` on macOS reports HIDIdleTime in nanoseconds.
pub fn parse_hid_idle(s: &str) -> Option<u64> {
    let line = s.lines().find(|l| l.contains("\"HIDIdleTime\""))?;
    let ns: u64 = line.rsplit('=').next()?.trim().parse().ok()?;
    Some(ns / 1_000_000_000)
}

fn idle_secs() -> Option<u64> {
    if cfg!(target_os = "macos") {
        return parse_hid_idle(&output("ioreg", &["-c", "IOHIDSystem", "-d", "4"])?);
    }
    None
}

/// Measures what the rules in force need. Each measurement runs a small
/// system command, so only the ones a rule depends on are taken.
pub fn probe(rules: &Rules, runtime: &str, own_requests: usize) -> Probe {
    use chrono::Timelike;
    let local = chrono::Local::now();
    let nvidia = if rules.when_gpu_busy {
        crate::telemetry::nvidia_status()
    } else {
        Vec::new()
    };
    Probe {
        now: local.timestamp(),
        minute_of_day: Some(local.hour() * 60 + local.minute()),
        on_battery: if rules.on_battery { on_battery() } else { None },
        other_gpu_mb: if rules.when_gpu_busy && !nvidia.is_empty() {
            other_gpu_mb(runtime)
        } else {
            None
        },
        gpu_utilisation_pct: nvidia
            .iter()
            .map(|g| g.utilization_pct)
            .fold(None, |m: Option<f64>, u| Some(m.map_or(u, |m| m.max(u)))),
        own_requests,
        idle_secs: if rules.when_active { idle_secs() } else { None },
    }
}

/// Turns what the rules say, moment to moment, into a steady answer.
///
/// Giving the machine back happens at once: the owner should not wait. The
/// exception is a busy GPU, which is believed only when it has been busy for
/// several looks in a row, because a GPU is briefly busy for many reasons.
/// Taking work again waits until the reason has been gone for a while, so a
/// charger nudged in its socket does not reload a model three times. An
/// explicit `resume` is the owner speaking and is obeyed at once.
#[derive(Default)]
pub struct Steady {
    current: Option<String>,
    busy_looks: u32,
    clear_looks: u32,
}

/// Looks in a row before a busy GPU is believed, and before a reason that
/// has gone is believed gone.
const BUSY_LOOKS: u32 = 3;
const CLEAR_LOOKS: u32 = 4;

impl Steady {
    pub fn current(&self) -> Option<&str> {
        self.current.as_deref()
    }

    /// Takes one look and returns the steady answer.
    pub fn look(&mut self, rules: &Rules, probe: &Probe) -> Option<String> {
        let mut now = decide(rules, probe);
        let busy = now
            .as_deref()
            .is_some_and(|r| r.starts_with("the GPU is in use"));
        self.busy_looks = if busy { self.busy_looks + 1 } else { 0 };
        if busy && self.current.is_none() && self.busy_looks < BUSY_LOOKS {
            now = None;
        }
        match now {
            Some(reason) => {
                self.clear_looks = 0;
                self.current = Some(reason);
            }
            None => {
                let was_pause = self
                    .current
                    .as_deref()
                    .is_some_and(|r| r.starts_with("paused"));
                self.clear_looks += 1;
                if was_pause || self.clear_looks >= CLEAR_LOOKS {
                    self.current = None;
                }
            }
        }
        self.current.clone()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn at(minute: u32) -> Probe {
        Probe {
            now: 1_000_000,
            minute_of_day: Some(minute),
            ..Default::default()
        }
    }

    #[test]
    fn the_defaults_give_way_on_battery_and_to_other_programs_only() {
        let rules = Rules::default();
        assert_eq!(decide(&rules, &at(600)), None);
        let mut p = at(600);
        p.on_battery = Some(true);
        assert_eq!(decide(&rules, &p).as_deref(), Some("on battery"));
        let mut p = at(600);
        p.other_gpu_mb = Some(6000);
        assert!(decide(&rules, &p).unwrap().contains("another program"));
        // The keyboard is not watched unless the owner asks.
        let mut p = at(600);
        p.idle_secs = Some(1);
        assert_eq!(decide(&rules, &p), None);
    }

    #[test]
    fn what_cannot_be_measured_is_not_guessed() {
        // A desktop has no battery; a Mac has no nvidia-smi.
        let rules = Rules {
            when_active: true,
            ..Default::default()
        };
        assert_eq!(decide(&rules, &at(600)), None);
    }

    #[test]
    fn a_busy_gpu_is_only_someone_elses_while_we_are_idle() {
        let rules = Rules::default();
        let mut p = at(600);
        p.gpu_utilisation_pct = Some(95.0);
        p.own_requests = 2;
        assert_eq!(decide(&rules, &p), None, "that is our own work");
        p.own_requests = 0;
        assert!(decide(&rules, &p).is_some());
        p.gpu_utilisation_pct = Some(4.0);
        assert_eq!(decide(&rules, &p), None);
    }

    #[test]
    fn a_rule_the_owner_turned_off_does_not_fire() {
        let rules = Rules {
            on_battery: false,
            when_gpu_busy: false,
            ..Default::default()
        };
        let mut p = at(600);
        p.on_battery = Some(true);
        p.other_gpu_mb = Some(9000);
        p.gpu_utilisation_pct = Some(99.0);
        assert_eq!(decide(&rules, &p), None);
    }

    #[test]
    fn a_pause_lasts_as_long_as_it_was_asked_for() {
        let mut rules = Rules {
            paused_until: Some(1_000_000 + 2 * 3600),
            ..Default::default()
        };
        assert_eq!(
            decide(&rules, &at(600)).as_deref(),
            Some("paused by its owner for another 2 hours")
        );
        let mut later = at(600);
        later.now = 1_000_000 + 2 * 3600;
        assert_eq!(decide(&rules, &later), None, "the pause has run out");
        rules.paused_until = Some(UNTIL_RESUMED);
        later.now = i64::MAX - 1;
        assert_eq!(
            decide(&rules, &later).as_deref(),
            Some("paused by its owner")
        );
    }

    #[test]
    fn hours_may_run_past_midnight() {
        assert_eq!(parse_hours("22:00-07:00"), Some((1320, 420)));
        assert_eq!(parse_hours(" 9:30 - 18:00 "), Some((570, 1080)));
        assert_eq!(parse_hours("25:00-07:00"), None);
        assert_eq!(parse_hours("nonsense"), None);
        assert!(within(1320, 420, 1380)); // 23:00
        assert!(within(1320, 420, 60)); // 01:00
        assert!(!within(1320, 420, 720)); // noon
        assert!(within(570, 1080, 600));
        assert!(!within(570, 1080, 1080), "the end is not included");
        assert!(within(0, 0, 777), "the same time twice is the whole day");

        let rules = Rules {
            hours: Some("22:00-07:00".into()),
            ..Default::default()
        };
        assert!(decide(&rules, &at(720))
            .unwrap()
            .contains("outside the hours"));
        assert_eq!(decide(&rules, &at(1400)), None);
    }

    #[test]
    fn durations_are_read_the_way_people_write_them() {
        assert_eq!(parse_duration("2h").unwrap().as_secs(), 7200);
        assert_eq!(parse_duration("30m").unwrap().as_secs(), 1800);
        assert_eq!(parse_duration("45").unwrap().as_secs(), 2700);
        assert_eq!(parse_duration("1d").unwrap().as_secs(), 86_400);
        assert!(parse_duration("soon").is_err());
        assert!(parse_duration("0h").is_err());
        assert!(parse_duration("3 fortnights").is_err());
    }

    #[test]
    fn reads_what_the_systems_report() {
        assert_eq!(
            parse_pmset(
                "Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1)\t80%; discharging;"
            ),
            Some(true)
        );
        assert_eq!(parse_pmset("Now drawing from 'AC Power'\n"), Some(false));
        assert_eq!(parse_pmset(""), None);

        let apps = "C:\\Games\\game.exe, 5120\n/usr/local/bin/ollama, 4300\n";
        assert_eq!(parse_compute_apps(apps, "ollama"), 5120);
        assert_eq!(parse_compute_apps("/usr/bin/python3, 20000\n", "vllm"), 0);
        assert_eq!(
            parse_compute_apps("/usr/bin/python3, 20000\n", "ollama"),
            20000
        );
        assert_eq!(parse_compute_apps("", "ollama"), 0);

        assert_eq!(
            parse_hid_idle("    |   \"HIDIdleTime\" = 42000000000\n"),
            Some(42)
        );
        assert_eq!(parse_hid_idle("nothing here"), None);
    }

    #[test]
    fn rules_survive_being_written_and_missing_fields_take_the_defaults() {
        let dir = std::env::temp_dir().join(format!("ayeusann-yield-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        assert_eq!(load(&dir), Rules::default(), "no file means the defaults");
        let rules = Rules {
            hours: Some("22:00-07:00".into()),
            paused_until: Some(UNTIL_RESUMED),
            when_active: true,
            ..Default::default()
        };
        save(&dir, &rules).unwrap();
        assert_eq!(load(&dir), rules);
        // A file written by hand, or by an older agent, with one field.
        std::fs::write(path(&dir), br#"{"when_active": true}"#).unwrap();
        let partial = load(&dir);
        assert!(partial.on_battery && partial.when_gpu_busy && partial.when_active);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn giving_way_is_immediate_and_coming_back_is_not() {
        let rules = Rules::default();
        let mut s = Steady::default();
        let mut battery = at(600);
        battery.on_battery = Some(true);

        assert_eq!(s.look(&rules, &battery).as_deref(), Some("on battery"));
        // Plugged in again: not believed at once.
        for _ in 0..CLEAR_LOOKS - 1 {
            assert!(s.look(&rules, &at(600)).is_some(), "came back too soon");
        }
        assert_eq!(s.look(&rules, &at(600)), None);
        // A charger nudged in its socket starts the wait again.
        s.look(&rules, &battery);
        s.look(&rules, &at(600));
        s.look(&rules, &battery);
        for _ in 0..CLEAR_LOOKS - 1 {
            assert!(s.look(&rules, &at(600)).is_some());
        }
        assert_eq!(s.look(&rules, &at(600)), None);
    }

    #[test]
    fn a_busy_gpu_is_believed_only_when_it_stays_busy() {
        let rules = Rules::default();
        let mut s = Steady::default();
        let mut busy = at(600);
        busy.gpu_utilisation_pct = Some(90.0);

        // One busy look, then quiet: nothing happens.
        assert_eq!(s.look(&rules, &busy), None);
        assert_eq!(s.look(&rules, &at(600)), None);
        // Busy look after look: believed.
        for _ in 0..BUSY_LOOKS - 1 {
            assert_eq!(s.look(&rules, &busy), None);
        }
        assert!(s.look(&rules, &busy).is_some());
    }

    #[test]
    fn resume_is_obeyed_at_once() {
        let mut rules = Rules {
            paused_until: Some(UNTIL_RESUMED),
            ..Default::default()
        };
        let mut s = Steady::default();
        assert!(s.look(&rules, &at(600)).is_some());
        rules.paused_until = None;
        assert_eq!(s.look(&rules, &at(600)), None, "the owner said resume");
    }
}
