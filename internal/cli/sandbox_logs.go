package cli

// SandboxLogsCommand is the shell command, run in a sandbox's launcher, that
// prints its logs: the guest console, then the output of a workload run over
// vsock in a checked-out warm slot. swiftletd keeps that output in its own file
// because Cloud Hypervisor writes the console log at its own offset and
// overwrote anything appended to it. follow tails both from the start and keeps
// waiting for either to appear.
func SandboxLogsCommand(runDir string, follow bool) string {
	console := runDir + "/serial.sock.log"
	workload := runDir + "/workload.log"
	if follow {
		return "tail -q -n +1 -F " + console + " " + workload
	}
	return "cat " + console + " && { cat " + workload + " 2>/dev/null || true; }"
}
