# swiftctl-ssh Specification

## Purpose

Add `swiftctl ssh <guest>` to connect to a Running SwiftGuest VM via SSH, using `status.network.primaryIP` and exec into the launcher pod. Mirrors the UX of `swiftctl console` for SSH access.

## ADDED Requirements

### Requirement: swiftctl ssh command

swiftctl MUST provide an `ssh` command that connects to the guest VM via SSH. The command MUST resolve the SwiftGuest, verify phase is Running, read `status.network.primaryIP`, exec into the launcher container, and run `ssh` with the given user and identity. It MUST stream stdin/stdout/stderr with TTY for interactive use.

#### Scenario: SSH connects when guest is Running and primaryIP is set

- **WHEN** an operator runs `swiftctl ssh <guest>` and the guest phase is Running and `status.network.primaryIP` is populated
- **THEN** swiftctl execs into the launcher container and runs `ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i <identity> <user>@<primaryIP>` with TTY streaming

#### Scenario: SSH fails when guest not Running

- **WHEN** an operator runs `swiftctl ssh <guest>` and the guest phase is not Running
- **THEN** swiftctl exits with a clear error message and non-zero exit code

#### Scenario: SSH fails when primaryIP not set

- **WHEN** an operator runs `swiftctl ssh <guest>` and `status.network.primaryIP` is empty or status.Network is nil
- **THEN** swiftctl exits with a clear error message and non-zero exit code

#### Scenario: User and identity flags

- **WHEN** an operator runs `swiftctl ssh -u myuser -i /path/to/key <guest>`
- **THEN** swiftctl uses `myuser` and `/path/to/key` for the SSH command

#### Scenario: Default user and identity

- **WHEN** an operator runs `swiftctl ssh <guest>` without -u or -i
- **THEN** swiftctl uses default user `kubeswift` and default identity `~/.ssh/id_rsa` (with ~ expanded to home directory)
