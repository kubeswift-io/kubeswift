# swiftctl-operability Specification

## Purpose

TBD - created by archiving change add-swiftctl-operability-commands. Update Purpose after archive.

## ADDED Requirements

### Requirement: swiftctl ssh access

swiftctl MUST provide an `ssh` command that connects to a Running SwiftGuest VM via SSH. It MUST use exec into the launcher pod to run the ssh client, using `status.network.primaryIP` for the target. It MUST support `--user` and `--identity` flags and MUST stream TTY for interactive use.

#### Scenario: SSH attaches when guest is Running

- **WHEN** an operator runs `swiftctl ssh <guest>` and the guest phase is Running and primaryIP is set
- **THEN** swiftctl resolves the guest, execs into the launcher container, runs ssh to the primaryIP, and streams stdin/stdout/stderr with TTY

#### Scenario: SSH fails when guest not Running

- **WHEN** an operator runs `swiftctl ssh <guest>` and the guest phase is not Running
- **THEN** swiftctl exits with a clear error and non-zero exit code
