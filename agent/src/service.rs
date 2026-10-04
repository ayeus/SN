//! Running the agent in the background, started again at every login.
//!
//! Each platform has its own way to do this, and none of them needs
//! administrator rights:
//!
//!   macOS    a launchd agent in ~/Library/LaunchAgents
//!   Linux    a systemd user unit in ~/.config/systemd/user
//!   Windows  the user's Run registry key
//!
//! The service definition carries no settings and no secrets. It starts the
//! agent with its data directory, and the agent reads the coordinator address,
//! region, runtime and any waiting registration token from its state file.

use crate::state;
use anyhow::{bail, Context, Result};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::Duration;

/// Exit code for "this machine has no service manager we can use". Installers
/// fall back to running the agent in the terminal when they see it.
pub const EXIT_UNSUPPORTED: i32 = 3;

/// What `service install` remembers from the command line.
#[derive(Default)]
pub struct Settings {
    pub token: Option<String>,
    pub coordinator: Option<String>,
    pub region: Option<String>,
    pub runtime: Option<String>,
    pub runtime_url: Option<String>,
    pub fake_gpu: bool,
    pub reset: bool,
}

/// One background agent: what to run and where it keeps its files.
pub struct Spec {
    /// Unique per agent instance, e.g. `io.ayeusann.agent`.
    pub label: String,
    pub exe: PathBuf,
    pub args: Vec<String>,
    pub data_dir: PathBuf,
    pub log: PathBuf,
}

pub struct Status {
    /// Registered to start at login.
    pub installed: bool,
    pub running: bool,
}

impl Spec {
    /// `base_dir` is the directory passed as --data-dir (or its default);
    /// `data_dir` is where this instance's files actually live.
    pub fn new(base_dir: &Path, data_dir: &Path, instance: Option<&str>) -> Result<Spec> {
        let base = std::path::absolute(base_dir).context("resolving the data directory")?;
        // The service always runs the copy kept next to the agent's data (see
        // `install_binary`), wherever this command was started from.
        let exe = base
            .join("bin")
            .join(format!("ayeusann-agent{}", std::env::consts::EXE_SUFFIX));
        let data_dir = std::path::absolute(data_dir).context("resolving the data directory")?;
        let log = data_dir.join("agent.log");

        let mut label = "io.ayeusann.agent".to_string();
        let mut args = vec![
            "--data-dir".to_string(),
            base.to_string_lossy().into_owned(),
            "--log-file".to_string(),
            log.to_string_lossy().into_owned(),
            "--background".to_string(),
        ];
        if let Some(i) = instance {
            let safe: String = i
                .chars()
                .map(|c| {
                    if c.is_ascii_alphanumeric() {
                        c.to_ascii_lowercase()
                    } else {
                        '-'
                    }
                })
                .collect();
            label = format!("{label}.{safe}");
            args.push("--instance".to_string());
            args.push(i.to_string());
        }
        Ok(Spec {
            label,
            exe,
            args,
            data_dir,
            log,
        })
    }
}

// ─── Commands ─────────────────────────────────────────────────

/// `service install`: remember the settings, register the service, start it,
/// and wait until it has connected. Returns the process exit code.
pub fn install(spec: &Spec, settings: Settings) -> i32 {
    if let Err(why) = platform::available() {
        eprintln!("Cannot run in the background on this machine: {why}");
        eprintln!("Run the agent in a terminal instead, or under your own process supervisor.");
        return EXIT_UNSUPPORTED;
    }

    if settings.reset {
        state::clear(&spec.data_dir);
    }
    let mut st = state::load(&spec.data_dir);
    if let Some(t) = settings.token {
        // A new token means a new enrolment.
        st.host_credential = None;
        st.pending_token = Some(t);
    }
    if let Some(c) = settings.coordinator {
        st.coordinator_url = Some(c);
    }
    if let Some(r) = settings.region {
        st.region = Some(r);
    }
    if let Some(r) = settings.runtime {
        st.runtime = Some(r);
    }
    if let Some(u) = settings.runtime_url {
        st.runtime_url = Some(u);
    }
    if settings.fake_gpu {
        st.fake_gpu = Some(true);
    }
    st.last_error = None;
    let enrolled = st.host_credential.as_deref().is_some_and(|c| !c.is_empty());
    if !enrolled && st.pending_token.is_none() {
        eprintln!("This machine is not enrolled yet. Pass --token <registration token> from the console (Hosts > Add a machine).");
        return 1;
    }
    if let Err(e) = state::save(&spec.data_dir, &st) {
        eprintln!("Could not save settings: {e:#}");
        return 1;
    }

    if let Err(e) = install_binary(spec) {
        eprintln!(
            "Could not install the agent to {}: {e:#}",
            spec.exe.display()
        );
        return 1;
    }
    if let Err(e) = platform::install(spec) {
        eprintln!("Could not set up the background service: {e:#}");
        return 1;
    }

    // Wait for it to connect, so a wrong token or an unreachable coordinator
    // is reported here rather than discovered later in a log file.
    let mut connected = false;
    let mut refused = None;
    for _ in 0..80 {
        std::thread::sleep(Duration::from_millis(500));
        let st = state::load(&spec.data_dir);
        if let Some(reason) = st.last_error {
            refused = Some(reason);
            break;
        }
        if st.host_id.is_some() && st.pending_token.is_none() && platform::status(spec).running {
            connected = true;
            break;
        }
    }

    if let Some(reason) = refused {
        let _ = platform::uninstall(spec);
        eprintln!("The coordinator refused this machine: {reason}");
        eprintln!("The background service was not kept. Fix the cause and run this command again.");
        return 1;
    }
    if !platform::status(spec).running {
        let tail = log_tail(&spec.log, 12);
        let _ = platform::uninstall(spec);
        eprintln!(
            "The agent stopped right after starting, so the background service was not kept."
        );
        if !tail.is_empty() {
            eprintln!("Its last output:\n{tail}");
        }
        return 1;
    }

    let st = state::load(&spec.data_dir);
    if connected {
        println!("The agent is running in the background and will start again at every login.");
    } else {
        println!(
            "The agent is running in the background, but has not reached the coordinator yet."
        );
        println!(
            "It keeps retrying. Check that this machine can reach {}.",
            st.coordinator_url.as_deref().unwrap_or("the coordinator")
        );
    }
    println!("  Log      {}", spec.log.display());
    println!("  Status   {} service status", exe_name(&spec.exe));
    println!("  Remove   {} service uninstall", exe_name(&spec.exe));
    0
}

/// `service uninstall`: stop the agent and stop starting it at login. The
/// enrolment is kept, so `service install` brings the same host back.
pub fn uninstall(spec: &Spec) -> i32 {
    let was = platform::status(spec);
    if let Err(e) = platform::uninstall(spec) {
        eprintln!("Could not remove the background service: {e:#}");
        return 1;
    }
    if was.installed || was.running {
        println!("The background agent is stopped and will no longer start at login.");
        println!(
            "This machine stays enrolled. Start it again with: {} service install",
            exe_name(&spec.exe)
        );
    } else {
        println!("No background agent was set up.");
    }
    0
}

/// `service status`. Exits 0 when the agent is running in the background.
pub fn status(spec: &Spec) -> i32 {
    let s = platform::status(spec);
    let st = state::load(&spec.data_dir);
    println!(
        "  Background service   {}",
        match (s.installed, s.running) {
            (true, true) => "running, starts at login",
            (true, false) => "set up, but not running",
            (false, true) => "running, but not set to start at login",
            (false, false) => "not set up",
        }
    );
    println!(
        "  Host id              {}",
        st.host_id.as_deref().unwrap_or("not enrolled")
    );
    if let Some(c) = &st.coordinator_url {
        println!("  Coordinator          {c}");
    }
    if let Some(e) = &st.last_error {
        println!("  Last refusal         {e}");
    }
    println!("  Log                  {}", spec.log.display());
    if s.running {
        0
    } else {
        1
    }
}

/// Puts a copy of the running binary where the service will run it from.
///
/// The copy matters when the agent was started from a build directory or a
/// download folder: those move, and macOS does not let a background process
/// run from Documents, Desktop or Downloads without a permission prompt that
/// nobody is there to answer, so the agent would hang before starting.
fn install_binary(spec: &Spec) -> Result<()> {
    let current = std::env::current_exe().context("locating the agent binary")?;
    let same = match (current.canonicalize(), spec.exe.canonicalize()) {
        (Ok(a), Ok(b)) => a == b,
        _ => false,
    };
    if same {
        return Ok(());
    }
    let dir = spec.exe.parent().unwrap();
    std::fs::create_dir_all(dir)?;
    // Copy to a temporary name and rename, so a running agent's binary is
    // replaced in one step rather than overwritten while in use.
    let tmp = dir.join(format!(".ayeusann-agent-{}.tmp", std::process::id()));
    std::fs::copy(&current, &tmp)?;
    if let Err(e) = std::fs::rename(&tmp, &spec.exe) {
        let _ = std::fs::remove_file(&tmp);
        // Windows will not replace a binary that is running. The one already
        // there is good enough to start; it is refreshed on the next install.
        if !spec.exe.exists() {
            return Err(e.into());
        }
    }
    Ok(())
}

fn exe_name(exe: &Path) -> String {
    exe.display().to_string()
}

fn log_tail(path: &Path, lines: usize) -> String {
    let text = std::fs::read_to_string(path).unwrap_or_default();
    let all: Vec<&str> = text.lines().collect();
    all[all.len().saturating_sub(lines)..].join("\n")
}

/// Runs a command and returns its trimmed stdout, or the reason it failed.
fn run(cmd: &str, args: &[&str]) -> Result<String> {
    let out = Command::new(cmd)
        .args(args)
        .output()
        .with_context(|| format!("running {cmd}"))?;
    if !out.status.success() {
        let err = String::from_utf8_lossy(&out.stderr);
        let msg = if err.trim().is_empty() {
            String::from_utf8_lossy(&out.stdout)
        } else {
            err
        };
        bail!("{cmd} {}: {}", args.join(" "), msg.trim());
    }
    Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
}

#[cfg(any(target_os = "macos", target_os = "linux"))]
fn home() -> Result<PathBuf> {
    std::env::var_os("HOME")
        .filter(|v| !v.is_empty())
        .map(PathBuf::from)
        .context("the home directory is not set")
}

// ─── macOS: launchd ───────────────────────────────────────────

/// The launchd property list for an agent. launchd restarts it whenever it
/// exits with an error, at most once every 30 seconds.
#[cfg(any(target_os = "macos", test))]
pub fn launchd_plist(spec: &Spec) -> String {
    fn esc(s: &str) -> String {
        s.replace('&', "&amp;")
            .replace('<', "&lt;")
            .replace('>', "&gt;")
    }
    let mut args = format!(
        "    <string>{}</string>\n",
        esc(&spec.exe.to_string_lossy())
    );
    for a in &spec.args {
        args.push_str(&format!("    <string>{}</string>\n", esc(a)));
    }
    format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{label}</string>
  <key>ProgramArguments</key>
  <array>
{args}  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>ProcessType</key>
  <string>Background</string>
</dict>
</plist>
"#,
        label = esc(&spec.label),
    )
}

#[cfg(target_os = "macos")]
mod platform {
    use super::*;

    fn plist_path(spec: &Spec) -> Result<PathBuf> {
        Ok(home()?
            .join("Library/LaunchAgents")
            .join(format!("{}.plist", spec.label)))
    }

    /// The launchd domain of the logged-in user, e.g. `gui/501`.
    fn domain() -> Result<String> {
        Ok(format!("gui/{}", run("id", &["-u"])?))
    }

    pub fn available() -> std::result::Result<(), String> {
        domain().map(|_| ()).map_err(|e| format!("{e:#}"))
    }

    pub fn install(spec: &Spec) -> Result<()> {
        let path = plist_path(spec)?;
        std::fs::create_dir_all(path.parent().unwrap())?;
        std::fs::write(&path, launchd_plist(spec))?;

        let domain = domain()?;
        let target = format!("{domain}/{}", spec.label);
        let _ = run("launchctl", &["bootout", &target]);
        // launchd may still be tearing the old job down; give it a moment.
        let mut last = None;
        for _ in 0..10 {
            match run(
                "launchctl",
                &["bootstrap", &domain, &path.to_string_lossy()],
            ) {
                Ok(_) => return Ok(()),
                Err(e) => last = Some(e),
            }
            std::thread::sleep(Duration::from_millis(500));
        }
        Err(last.unwrap())
    }

    pub fn uninstall(spec: &Spec) -> Result<()> {
        if let Ok(domain) = domain() {
            let _ = run(
                "launchctl",
                &["bootout", &format!("{domain}/{}", spec.label)],
            );
        }
        let path = plist_path(spec)?;
        if path.exists() {
            std::fs::remove_file(&path)?;
        }
        Ok(())
    }

    pub fn status(spec: &Spec) -> Status {
        let installed = plist_path(spec).map(|p| p.exists()).unwrap_or(false);
        let running = domain()
            .and_then(|d| run("launchctl", &["print", &format!("{d}/{}", spec.label)]))
            .map(|out| out.lines().any(|l| l.trim() == "state = running"))
            .unwrap_or(false);
        Status { installed, running }
    }
}

// ─── Linux: systemd user unit ─────────────────────────────────

/// The systemd unit for an agent. systemd restarts it when it exits with an
/// error, after 30 seconds.
#[cfg(any(target_os = "linux", test))]
pub fn systemd_unit(spec: &Spec) -> String {
    // systemd reads % and $ specially inside ExecStart, and splits on spaces.
    fn quote(s: &str) -> String {
        let escaped = s
            .replace('\\', "\\\\")
            .replace('"', "\\\"")
            .replace('%', "%%")
            .replace('$', "$$");
        format!("\"{escaped}\"")
    }
    let mut cmd = quote(&spec.exe.to_string_lossy());
    for a in &spec.args {
        cmd.push(' ');
        cmd.push_str(&quote(a));
    }
    format!(
        "[Unit]\n\
         Description=AyeusANN host agent\n\
         After=network-online.target\n\
         Wants=network-online.target\n\
         \n\
         [Service]\n\
         ExecStart={cmd}\n\
         Restart=on-failure\n\
         RestartSec=30\n\
         \n\
         [Install]\n\
         WantedBy=default.target\n"
    )
}

#[cfg(target_os = "linux")]
mod platform {
    use super::*;

    fn unit_name(spec: &Spec) -> String {
        // io.ayeusann.agent[.instance] → ayeusann-agent[-instance].service
        let suffix = spec.label.strip_prefix("io.ayeusann.agent").unwrap_or("");
        format!("ayeusann-agent{}.service", suffix.replace('.', "-"))
    }

    fn unit_path(spec: &Spec) -> Result<PathBuf> {
        let config = match std::env::var_os("XDG_CONFIG_HOME").filter(|v| !v.is_empty()) {
            Some(dir) => PathBuf::from(dir),
            None => home()?.join(".config"),
        };
        Ok(config.join("systemd/user").join(unit_name(spec)))
    }

    pub fn available() -> std::result::Result<(), String> {
        run("systemctl", &["--user", "show-environment"])
            .map(|_| ())
            .map_err(|_| {
                "it has no systemd user session (common in containers and in WSL without systemd)"
                    .to_string()
            })
    }

    pub fn install(spec: &Spec) -> Result<()> {
        let path = unit_path(spec)?;
        std::fs::create_dir_all(path.parent().unwrap())?;
        std::fs::write(&path, systemd_unit(spec))?;
        let unit = unit_name(spec);
        run("systemctl", &["--user", "daemon-reload"])?;
        run("systemctl", &["--user", "enable", &unit])?;
        run("systemctl", &["--user", "restart", &unit])?;
        // Without lingering, a user's services stop when they log out and only
        // start at their next login. Enabling it can need a password on some
        // systems; the agent still works without it.
        if let Ok(user) = std::env::var("USER") {
            let _ = run("loginctl", &["enable-linger", &user]);
        }
        Ok(())
    }

    pub fn uninstall(spec: &Spec) -> Result<()> {
        let unit = unit_name(spec);
        let _ = run("systemctl", &["--user", "disable", "--now", &unit]);
        let path = unit_path(spec)?;
        if path.exists() {
            std::fs::remove_file(&path)?;
        }
        let _ = run("systemctl", &["--user", "daemon-reload"]);
        Ok(())
    }

    pub fn status(spec: &Spec) -> Status {
        let unit = unit_name(spec);
        Status {
            installed: unit_path(spec).map(|p| p.exists()).unwrap_or(false),
            running: run("systemctl", &["--user", "is-active", &unit]).is_ok(),
        }
    }
}

// ─── Windows: the user's Run key ──────────────────────────────

/// The command line stored in the Run key. Arguments with spaces are quoted;
/// paths cannot contain quotes on Windows, so nothing needs escaping.
#[cfg(any(windows, test))]
pub fn windows_command_line(spec: &Spec) -> String {
    let quote = |s: &str| {
        if s.is_empty() || s.contains(' ') {
            format!("\"{s}\"")
        } else {
            s.to_string()
        }
    };
    let mut cmd = quote(&spec.exe.to_string_lossy());
    for a in &spec.args {
        cmd.push(' ');
        cmd.push_str(&quote(a));
    }
    cmd
}

#[cfg(windows)]
mod platform {
    use super::*;
    use std::os::windows::process::CommandExt;

    const RUN_KEY: &str = r"HKCU\Software\Microsoft\Windows\CurrentVersion\Run";
    // The child gets no console window and is not tied to this one.
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;

    fn pid(spec: &Spec) -> Option<u32> {
        std::fs::read_to_string(spec.data_dir.join("agent.pid"))
            .ok()?
            .trim()
            .parse()
            .ok()
    }

    fn alive(pid: u32) -> bool {
        run(
            "tasklist",
            &["/FI", &format!("PID eq {pid}"), "/NH", "/FO", "CSV"],
        )
        .map(|out| out.contains(&format!("\"{pid}\"")))
        .unwrap_or(false)
    }

    fn stop(spec: &Spec) {
        if let Some(pid) = pid(spec).filter(|p| alive(*p)) {
            let _ = run("taskkill", &["/PID", &pid.to_string(), "/F"]);
        }
        let _ = std::fs::remove_file(spec.data_dir.join("agent.pid"));
    }

    pub fn available() -> std::result::Result<(), String> {
        Ok(())
    }

    pub fn install(spec: &Spec) -> Result<()> {
        run(
            "reg",
            &[
                "add",
                RUN_KEY,
                "/v",
                &spec.label,
                "/t",
                "REG_SZ",
                "/d",
                &windows_command_line(spec),
                "/f",
            ],
        )?;
        stop(spec);
        Command::new(&spec.exe)
            .args(&spec.args)
            .creation_flags(CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP)
            .spawn()
            .context("starting the agent")?;
        Ok(())
    }

    pub fn uninstall(spec: &Spec) -> Result<()> {
        let _ = run("reg", &["delete", RUN_KEY, "/v", &spec.label, "/f"]);
        stop(spec);
        Ok(())
    }

    pub fn status(spec: &Spec) -> Status {
        Status {
            installed: run("reg", &["query", RUN_KEY, "/v", &spec.label]).is_ok(),
            running: pid(spec).is_some_and(alive),
        }
    }
}

// ─── Anything else ────────────────────────────────────────────

#[cfg(not(any(target_os = "macos", target_os = "linux", windows)))]
mod platform {
    use super::*;

    pub fn available() -> std::result::Result<(), String> {
        Err("background services are not supported on this operating system".into())
    }
    pub fn install(_: &Spec) -> Result<()> {
        bail!("unsupported")
    }
    pub fn uninstall(_: &Spec) -> Result<()> {
        Ok(())
    }
    pub fn status(_: &Spec) -> Status {
        Status {
            installed: false,
            running: false,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn spec() -> Spec {
        Spec {
            label: "io.ayeusann.agent".into(),
            exe: PathBuf::from("/Users/a b/.ayeusann/bin/ayeusann-agent"),
            args: vec![
                "--data-dir".into(),
                "/Users/a b/.ayeusann".into(),
                "--log-file".into(),
                "/Users/a b/.ayeusann/agent.log".into(),
                "--background".into(),
            ],
            data_dir: PathBuf::from("/Users/a b/.ayeusann"),
            log: PathBuf::from("/Users/a b/.ayeusann/agent.log"),
        }
    }

    #[test]
    fn launchd_plist_runs_the_agent_and_restarts_it_on_failure() {
        let p = launchd_plist(&spec());
        assert!(p.contains("<string>io.ayeusann.agent</string>"));
        assert!(p.contains("<string>/Users/a b/.ayeusann/bin/ayeusann-agent</string>"));
        assert!(
            p.contains("<string>--data-dir</string>\n    <string>/Users/a b/.ayeusann</string>")
        );
        assert!(p.contains("<key>RunAtLoad</key>\n  <true/>"));
        assert!(p.contains("<key>SuccessfulExit</key>\n    <false/>"));
    }

    #[test]
    fn launchd_plist_escapes_xml() {
        let mut s = spec();
        s.args.push("a<b>&c".into());
        assert!(launchd_plist(&s).contains("<string>a&lt;b&gt;&amp;c</string>"));
    }

    #[test]
    fn systemd_unit_quotes_every_argument() {
        let mut s = spec();
        s.args.push("100%$HOME".into());
        let u = systemd_unit(&s);
        assert!(u.contains(
            "ExecStart=\"/Users/a b/.ayeusann/bin/ayeusann-agent\" \"--data-dir\" \"/Users/a b/.ayeusann\""
        ));
        assert!(u.contains("\"100%%$$HOME\""), "{u}");
        assert!(u.contains("Restart=on-failure"));
        assert!(u.contains("WantedBy=default.target"));
    }

    #[test]
    fn windows_command_line_quotes_only_what_needs_it() {
        let mut s = spec();
        s.exe = PathBuf::from(r"C:\Users\A B\.ayeusann\bin\ayeusann-agent.exe");
        s.args = vec![
            "--data-dir".into(),
            r"C:\Users\A B\.ayeusann".into(),
            "--background".into(),
        ];
        assert_eq!(
            windows_command_line(&s),
            r#""C:\Users\A B\.ayeusann\bin\ayeusann-agent.exe" --data-dir "C:\Users\A B\.ayeusann" --background"#
        );
    }

    #[test]
    fn instances_get_their_own_label_and_arguments() {
        let s = Spec::new(
            Path::new("/tmp/x"),
            Path::new("/tmp/x/instance-Dev 1"),
            Some("Dev 1"),
        )
        .unwrap();
        assert_eq!(s.label, "io.ayeusann.agent.dev-1");
        assert_eq!(s.exe, PathBuf::from("/tmp/x/bin/ayeusann-agent"));
        assert!(s
            .args
            .ends_with(&["--instance".to_string(), "Dev 1".to_string()]));
        assert_eq!(s.log, PathBuf::from("/tmp/x/instance-Dev 1/agent.log"));
    }
}
