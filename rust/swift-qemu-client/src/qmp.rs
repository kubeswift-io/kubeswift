//! Minimal synchronous QMP (QEMU Machine Protocol) client.
//!
//! Only implements lifecycle commands: capabilities negotiation, system_powerdown, quit.
//! Each public method opens a fresh connection — QEMU QMP is stateless enough for this.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::time::Duration;

pub struct QmpClient {
    socket_path: PathBuf,
}

impl QmpClient {
    pub fn new(socket_path: PathBuf) -> Self {
        Self { socket_path }
    }

    /// Open a QMP connection, negotiate capabilities, execute `cmd`, read the ack.
    ///
    /// QMP session:
    ///   S→C: {"QMP": {"version": {...}, "capabilities": [...]}}
    ///   C→S: {"execute": "qmp_capabilities"}
    ///   S→C: {"return": {}}
    ///   C→S: {"execute": "<cmd>"}
    ///   S→C: {"return": {}}
    fn execute(&self, cmd: &str) -> Result<(), String> {
        let stream = UnixStream::connect(&self.socket_path)
            .map_err(|e| format!("QMP connect {:?}: {}", self.socket_path, e))?;
        stream
            .set_read_timeout(Some(Duration::from_secs(5)))
            .map_err(|e| format!("QMP set_read_timeout: {}", e))?;

        // BufReader borrows &stream for Read; &stream also implements Write.
        let mut reader = BufReader::new(&stream);
        let mut line = String::new();

        // Read server greeting
        reader
            .read_line(&mut line)
            .map_err(|e| format!("QMP read greeting: {}", e))?;
        log::debug!("qmp_greeting: {}", line.trim());

        // Negotiate capabilities
        (&stream)
            .write_all(b"{\"execute\":\"qmp_capabilities\"}\n")
            .map_err(|e| format!("QMP write qmp_capabilities: {}", e))?;
        line.clear();
        reader
            .read_line(&mut line)
            .map_err(|e| format!("QMP read capabilities response: {}", e))?;
        log::debug!("qmp_caps_ack: {}", line.trim());

        // Execute the requested command
        let payload = format!("{{\"execute\":\"{}\"}}\n", cmd);
        (&stream)
            .write_all(payload.as_bytes())
            .map_err(|e| format!("QMP write {}: {}", cmd, e))?;
        line.clear();
        reader
            .read_line(&mut line)
            .map_err(|e| format!("QMP read {} ack: {}", cmd, e))?;
        log::debug!("qmp_{}_ack: {}", cmd, line.trim());

        Ok(())
    }

    /// Request graceful ACPI power-down. Guest receives ACPI shutdown event and initiates
    /// its own shutdown sequence. The QEMU process exits after guest powers off.
    pub fn powerdown(&self) -> Result<(), String> {
        self.execute("system_powerdown")
    }

    /// Force-quit the QEMU process immediately (no guest notification).
    pub fn quit(&self) -> Result<(), String> {
        self.execute("quit")
    }
}
