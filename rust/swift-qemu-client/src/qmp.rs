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

    /// Like `execute`, but returns the command's response line for parsing.
    fn execute_query(&self, cmd: &str) -> Result<String, String> {
        let stream = UnixStream::connect(&self.socket_path)
            .map_err(|e| format!("QMP connect {:?}: {}", self.socket_path, e))?;
        stream
            .set_read_timeout(Some(Duration::from_secs(5)))
            .map_err(|e| format!("QMP set_read_timeout: {}", e))?;
        let mut reader = BufReader::new(&stream);
        let mut line = String::new();
        reader
            .read_line(&mut line)
            .map_err(|e| format!("QMP read greeting: {}", e))?;
        (&stream)
            .write_all(b"{\"execute\":\"qmp_capabilities\"}\n")
            .map_err(|e| format!("QMP write qmp_capabilities: {}", e))?;
        line.clear();
        reader
            .read_line(&mut line)
            .map_err(|e| format!("QMP read capabilities response: {}", e))?;
        let payload = format!("{{\"execute\":\"{}\"}}\n", cmd);
        (&stream)
            .write_all(payload.as_bytes())
            .map_err(|e| format!("QMP write {}: {}", cmd, e))?;
        line.clear();
        reader
            .read_line(&mut line)
            .map_err(|e| format!("QMP read {} response: {}", cmd, e))?;
        Ok(line)
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

    /// Map vCPU index → host thread id via query-cpus-fast. The thread ids are
    /// what sched_setaffinity pins (QEMU has no CLI for vCPU affinity).
    pub fn query_cpus_fast(&self) -> Result<Vec<(u32, i32)>, String> {
        let line = self.execute_query("query-cpus-fast")?;
        parse_cpus_fast(&line)
    }
}

/// Parse a query-cpus-fast response:
/// `{"return":[{"cpu-index":0,"thread-id":12345,...},...]}`.
pub(crate) fn parse_cpus_fast(line: &str) -> Result<Vec<(u32, i32)>, String> {
    let v: serde_json::Value =
        serde_json::from_str(line).map_err(|e| format!("query-cpus-fast parse: {}", e))?;
    let cpus = v
        .get("return")
        .and_then(|r| r.as_array())
        .ok_or_else(|| format!("query-cpus-fast: no return array in {}", line.trim()))?;
    let mut out = Vec::with_capacity(cpus.len());
    for c in cpus {
        let idx = c
            .get("cpu-index")
            .and_then(|x| x.as_u64())
            .ok_or_else(|| "query-cpus-fast: entry missing cpu-index".to_string())?;
        let tid = c
            .get("thread-id")
            .and_then(|x| x.as_i64())
            .ok_or_else(|| "query-cpus-fast: entry missing thread-id".to_string())?;
        out.push((idx as u32, tid as i32));
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::parse_cpus_fast;

    #[test]
    fn parse_cpus_fast_maps_index_to_thread() {
        let line = r#"{"return":[{"cpu-index":0,"qom-path":"/machine/unattached/device[0]","thread-id":4100,"target":"x86_64","props":{"core-id":0,"thread-id":0,"socket-id":0}},{"cpu-index":1,"qom-path":"/machine/unattached/device[2]","thread-id":4101,"target":"x86_64","props":{"core-id":1,"thread-id":0,"socket-id":0}}]}"#;
        let cpus = parse_cpus_fast(line).unwrap();
        assert_eq!(cpus, vec![(0, 4100), (1, 4101)]);
    }

    #[test]
    fn parse_cpus_fast_rejects_garbage() {
        assert!(parse_cpus_fast("{}").is_err());
        assert!(parse_cpus_fast("not json").is_err());
    }
}
