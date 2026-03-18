## Context

swiftctl already provides `console` for serial access and `start`/`stop`/`restart` for lifecycle. SSH access today requires the operator to read `status.network.primaryIP` and run `ssh` manually. The swiftguest-ssh-access spec documents this workflow. Adding `swiftctl ssh` provides a single-command path that mirrors `swiftctl console` in structure and UX.

## Goals / Non-Goals

**Goals:**
- Add `swiftctl ssh <guest>` that resolves the guest, reads primaryIP, and connects via SSH
- Follow the same pattern as `console.go` (GuestResolver, exec into launcher, TTY streaming)
- Support `--user` and `--identity` flags with sensible defaults

**Non-Goals:**
- No SSH key generation or injection (handled by SwiftSeedProfile / cloud-init)
- No changes to API types, controllers, or swiftletd

## Decisions

### Exec into launcher, run ssh client

**Decision:** Run `ssh` inside the launcher container via exec (same as console uses socat).

**Rationale:** The launcher pod has network access to the guest VM. Running ssh from the operator's machine would require the guest IP to be routable from the operator—which may not hold in typical cluster setups. Exec into launcher ensures the ssh client runs where the guest is reachable.

### Identity path resolution

**Decision:** The `--identity` path is resolved and used inside the launcher container. Default `~/.ssh/id_rsa` expands to the container's home (e.g., `/root/.ssh/id_rsa`).

**Rationale:** Operators who mount their SSH key into the launcher (e.g., via Secret volume) can use the default or `-i`. For operators without a mount, the identity must be available in the launcher—document that a Secret volume or similar is required for key injection. No key-copy-over-stdin in the initial implementation to keep scope small.

### Default user "kubeswift"

**Decision:** Default `--user` to `kubeswift`.

**Rationale:** Matches common cloud-init user naming. Operators override with `-u` for custom users.

### StrictHostKeyChecking=no, UserKnownHostsFile=/dev/null

**Decision:** Pass these options to ssh to avoid host key prompts.

**Rationale:** Guest IPs can change across restarts; host keys are not stable. Non-interactive first connection without prompts.

## Risks / Trade-offs

- **Risk:** Identity file must exist inside the launcher container at the given path. **Mitigation:** Document that operators may need to mount an SSH key Secret into the launcher (e.g., via SwiftSeedProfile or pod template). If the path does not exist, ssh will fail with a clear error.
- **Risk:** Guest must have `status.network.primaryIP` populated. **Mitigation:** Require phase Running and check Network != nil and PrimaryIP != ""; return clear error otherwise.
