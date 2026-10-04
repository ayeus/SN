//! Where the agent writes: the terminal, or a log file when it runs in the
//! background and has no terminal to write to.

use std::fs::{File, OpenOptions};
use std::io::{self, IsTerminal, Write};
use std::path::Path;
use std::sync::{Mutex, OnceLock};

static LOG: OnceLock<Mutex<File>> = OnceLock::new();

/// One previous log is kept, so the two files together stay under 10 MB.
const MAX_LOG_BYTES: u64 = 5 * 1024 * 1024;

/// Sends all further output to `path`, appending. A log that has grown past
/// the limit is set aside as `<name>.1` first.
pub fn to_file(path: &Path) -> io::Result<()> {
    if let Some(dir) = path.parent() {
        std::fs::create_dir_all(dir)?;
    }
    if std::fs::metadata(path)
        .map(|m| m.len() > MAX_LOG_BYTES)
        .unwrap_or(false)
    {
        let mut old = path.as_os_str().to_owned();
        old.push(".1");
        let _ = std::fs::rename(path, old);
    }
    let file = OpenOptions::new().create(true).append(true).open(path)?;
    let _ = LOG.set(Mutex::new(file));
    Ok(())
}

/// Whether output goes to a log file rather than a terminal.
pub fn is_file() -> bool {
    LOG.get().is_some()
}

/// Whether to colour output. A log file and the classic Windows console both
/// show escape codes literally.
pub fn colour() -> bool {
    !is_file() && !cfg!(windows) && io::stderr().is_terminal()
}

/// A writer for the current destination.
pub struct Out;

impl Write for Out {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        match LOG.get() {
            Some(file) => file.lock().unwrap_or_else(|e| e.into_inner()).write(buf),
            None => io::stderr().write(buf),
        }
    }

    fn flush(&mut self) -> io::Result<()> {
        match LOG.get() {
            Some(file) => file.lock().unwrap_or_else(|e| e.into_inner()).flush(),
            None => io::stderr().flush(),
        }
    }
}
