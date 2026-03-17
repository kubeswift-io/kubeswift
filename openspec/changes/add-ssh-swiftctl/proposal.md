## Why

Operators need a convenient way to SSH into SwiftGuest VMs. The workflow today requires manually reading `status.network.primaryIP` from the SwiftGuest and running `ssh` with the correct key and user. This is error-prone and inconsistent with how `swiftctl console` provides one-command access to the serial console. Adding `swiftctl ssh <guest>` provides parity: a single command that resolves the guest, discovers the IP, and connects via SSH—streamlining the operator experience and matching the documented SSH workflow in swiftguest-ssh-access.

## What Changes

- Add `swiftctl ssh <guest>` command in `cmd/swiftctl/ssh.go`
- Command resolves SwiftGuest, verifies phase == Running, reads `status.network.primaryIP`, execs into launcher pod, runs `ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i <identity> <user>@<primaryIP>`
- Flags: `--user` / `-u` (default: kubeswift), `--identity` / `-i` (default: ~/.ssh/id_rsa)
- Register `sshCmd` in `cmd/swiftctl/root.go`

## Capabilities

### New Capabilities

- `swiftctl-ssh`: Add `swiftctl ssh <guest>` command that connects to the guest VM via SSH using status.network.primaryIP, exec into launcher, and stream TTY.

### Modified Capabilities

- `swiftctl-operability`: Extend with ssh command requirement (swiftctl must provide ssh for SSH access workflow).

## Impact

- **Code**: `cmd/swiftctl/ssh.go` (new), `cmd/swiftctl/root.go` (add one line)
- **Dependencies**: Uses existing `cli.GuestResolver`, `cli.LauncherContainer`, `guest.Status.Network.PrimaryIP`
- **No API changes**: SwiftGuest, SwiftSeedProfile, and other API types unchanged
