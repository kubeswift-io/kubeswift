## 1. Add ssh command implementation

- [ ] 1.1 Create `cmd/swiftctl/ssh.go` with `sshCmd` cobra command, `ExactArgs(1)`, flags `--user`/`-u` (default: kubeswift) and `--identity`/`-i` (default: ~/.ssh/id_rsa)
- [ ] 1.2 Implement `runSSH`: get namespace via `getNamespace()`, config via `kubeConfig.ToRESTConfig()`, create controller-runtime client with scheme
- [ ] 1.3 Use `cli.GuestResolver` to resolve SwiftGuest and pod; verify `guest.Status.Phase == "Running"`
- [ ] 1.4 Check `guest.Status.Network != nil` and `guest.Status.Network.PrimaryIP != ""`; return clear error if missing
- [ ] 1.5 Expand `~` in identity path to home directory (os.UserHomeDir)
- [ ] 1.6 Build ssh command: `ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i <identity> <user>@<primaryIP>`
- [ ] 1.7 Use `kubernetes.NewForConfig`, `CoreV1().RESTClient().Post()`, `remotecommand.NewSPDYExecutor` with `cli.LauncherContainer`, TTY=true
- [ ] 1.8 Stream stdin/stdout/stderr with TTY (same pattern as console.go: term.MakeRaw, StreamWithContext, TerminalSizeQueue)

## 2. Register command

- [ ] 2.1 Add `rootCmd.AddCommand(sshCmd)` in `cmd/swiftctl/root.go`
