# lease

Some resources on a machine can only serve one caller at a time: an iOS
Simulator device, a dev-server port, a local Postgres, an Xcode build, a
self-hosted runner. When workers, skills, and CI jobs share the machine,
they bump into each other. `lease` gives each resource a named lock so
callers line up instead of colliding.

A lock is a kernel file lock (`flock(2)`) in a state directory. There is no
daemon to run. The kernel releases a lock when every process holding its
file descriptor has closed it or exited, so a crash or a `kill -9` cannot
leak a lock. Holder records spot stale holders and take them over
automatically.

```sh
lease run ios-simulator -- xcrun simctl boot "iPhone 16"
lease run dev-port-3000 -- ./scripts/dev-server
lease run db -- ./scripts/parallel-migration
lease status
```

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/delaanthonio/lease/main/install.sh | sh
```

This installs the `lease` binary for your platform into `~/.local/bin` (or
`/usr/local/bin` with `sudo`, or any directory with `INSTALL_DIR=...`).
Binaries are built for macOS and Linux (amd64, arm64). You can also build
from source with Go 1.25 or newer:

```sh
go install github.com/delaanthonio/lease/cmd/lease@latest
```

For a privileged install, every destination path component must be a
root-owned directory that is not group- or world-writable, and must not be
a symlink. Missing directories are created after their ancestors are checked.

## Quick start

Wrap the command that uses the scarce resource:

```sh
# One exclusive lock. A second caller waits by default,
# or fails right away with --no-wait.
lease run ios-simulator -- xcrun simctl boot "iPhone 16"

# Wait at most five minutes, then give up.
lease run db --wait 5m -- ./scripts/seed-db

# Counted slots: configure two slots, then each caller takes one.
mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/lease"
export LEASE_CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/lease/config.toml"
printf '[resources.build]\nslots = 2\n' > "$LEASE_CONFIG"
lease run build -- xcodebuild -scheme App build
```

`lease run` takes every named lock in a fixed sorted order, runs the
command with inherited stdio, closes its lock descriptors when the command
exits (including on signals), and returns the command's exit code. A child
that keeps an inherited descriptor keeps the matching lock held.

To see what is held right now:

```sh
lease status                 # every resource on this machine
lease status ios-simulator   # one resource
lease status --json          # stable, compact JSON for agents
```

```text
RESOURCE  HOLDER                       SINCE  SLOTS  WAITING
--------  ---------------------------  -----  -----  -------
db        user:alice pid 4242 seed-db  5m     1/1    1
```

## Use it from a task runner

In a mise task, wrap the same way:

```toml
[tasks.test-sim]
run = "lease run ios-simulator -- sh -c \"xcrun simctl boot 'iPhone 16' && ./scripts/ui-tests\""

[tasks.dev-server]
run = "lease run dev-port-3000 --wait 5m -- ./scripts/dev-server"
```

### Getting everyone to use it

A lock only helps when every user of the resource goes through `lease`. The
kernel cannot stop a script from calling `xcrun simctl boot` directly. A
small CI check keeps the convention visible by failing when a
scarce-resource command shows up outside `lease run`:

```sh
# scripts/check-lease-usage.sh - fail CI on direct calls that bypass lease
if grep -RInE '(^|[^-a-z])(xcrun simctl boot|maestro test)\b' \
    scripts/ tasks/ 2>/dev/null |
    grep -v 'lease run'; then
  echo "error: wrap scarce-resource commands in 'lease run'"
  exit 1
fi
```

Run it in CI alongside your other checks. It is a grep, not a proof, but it
keeps the convention visible.

## Commands

| command | what it does |
| --- | --- |
| `lease run NAME... -- CMD ARGS` | hold the locks while running a command |
| `lease status [NAME...] [--json]` | show holders, ages, slot usage, waiters |
| `lease acquire NAME...` | hold a lock manually; prints a release token |
| `lease release TOKEN` | release a manually acquired lock |
| `lease force-release NAME --reason TEXT` | clear a stale holder record |
| `lease config` | show resolved resources and slot counts |

### Manual acquire and release

```sh
token=$(lease acquire dev-port-3000)   # one slot, held by a small keeper process
...use the resource...
lease release "$token"
```

`lease acquire` prints one token for the complete set of resources and
slots. A small keeper process holds the kernel lock until the lock is
released, its owner exits, its TTL expires, or it is killed. A killed
keeper leaves its holder record behind for stale-holder takeover. By
default, if the calling script dies before releasing, its keeper releases
the lock.

### Force release

`lease force-release NAME --reason TEXT` clears locks that are stuck. It
removes holder records whose process is gone and writes the reason and the
previous holder to history. It never kills a live process: a slot held by a
running process stays held until that process exits. Restrict this command
to whoever runs the machine; everyday automation should not need it. That
restriction is a convention, not an authorization check by `lease`.

## Slots and configuration

An optional TOML file says how many callers a resource admits:

```toml
[resources.ios-simulator]
slots = 1

[resources.dev-port-3000]
slots = 1

[resources.build]
slots = 3
```

The file lives at `$LEASE_CONFIG`, or `lease/config.toml` under the user
config directory (`$XDG_CONFIG_HOME/lease/config.toml`, or
`~/.config/lease/config.toml` on Linux,
`~/Library/Application Support/lease/config.toml` on macOS). A resource
that is not in the file gets one exclusive slot. `lease config` shows what
was resolved.

## Holder identity

A holder is named from the environment: `FM_TASK_ID` for agent tasks,
`GITHUB_RUN_ID` and `GITHUB_JOB` for CI, otherwise the user, pid, and
command. The status view and busy messages report the holder and acquisition
time when a holder record is available.

## Exit codes

| code | meaning |
| --- | --- |
| 0 | success (or the command exited 0) |
| 1 | lease itself failed |
| 2 | usage error |
| 75 | the resource is busy or the wait elapsed (EX_TEMPFAIL) |
| n | the command's own exit code, `128+n` when it died from signal n |

A busy failure can name the current holder and when it acquired the lock:

```text
lease: resource "db" slot 1 is held by user:alice pid 4242 seed-db since 2026-11-07T09:12:00Z
```

## Where state lives

By default, state lives outside any worktree. The directory is chosen from
`$LEASE_DIR`, then `$LEASE_STATE_DIR`, then `dir` in the config, or `lease` under the user state dir
(`$XDG_STATE_HOME/lease` or `~/.local/state/lease` on Linux,
`~/Library/Application Support/lease` on macOS). Inside:

- `locks/<encoded-resource>/slot-N.lock` - the kernel lock files; holding a slot
  means holding an exclusive `flock` on one of these
- `holders/`, `waiters/` - small JSON records: who holds what, who waits
- `events.ndjson` - retained history, including successful force-releases

Stale holders (pid gone, or the pid was reused by a different process) are
detected and taken over automatically, with a note on stderr and a
best-effort entry in the history. For `lease run`, `--ttl` identifies an
old stale holder record during takeover; it does not release a live
holder's lock.

All metadata goes through an internal `Store` interface with the
file-backed implementation as the only one today; a database-backed
implementation (for history and stats) can replace it later without
changing the commands.

## Scope

lease is deliberately small: no scheduling, no priorities, no load-average
awareness, no killing of processes. It coordinates; it does not decide.

## Coordination and privacy

Workers, skills, and CI must run as the same user, or use the same `LEASE_DIR` and a common group. Set `dir` in the config as an alternative to `LEASE_DIR`; environment overrides take precedence. For a group-owned directory, set `shared = true` in every participant's config. New directories use mode 0770 and coordination files 0660; the default is 0700 and 0600. History is private (0600) by default and group-readable and writable (0660) in shared mode. The shared directory must be owned by the common group and have the setgid bit set so new files inherit that group. On Linux, a new shared directory under a parent without setgid can be used before its own setgid bit is applied, so children created during that window may inherit the wrong group. Every participant coordinating through the same state directory must configure identical slot counts for each resource, and slot counts should not change while leases are active: callers configuring different counts can both use a resource that should be exclusive.

Commands record only the executable basename and holder identity by default. Use `--record-command` or `record_command = true` to include arguments in private mode; shared mode refuses full command recording. History retains at most 1,000 events and 30 days, whichever is smaller. Appends and history reads prune expired entries; if a read encounters a busy history lock, it filters the results and leaves pruning on disk for a later operation. No cleanup runs while lease is idle. History is best-effort diagnostics, not an audit trail; events can be dropped under contention. Force-release records its reason before clearing a stale holder. Full command recording is off by default; files outside lease are not touched by retention cleanup.

Manual acquisitions use the calling parent as owner; `--owner-pid` selects another owner. The default TTL is 30 minutes, configurable with `manual_ttl = "30m"` or `--ttl`. `lease release NAME` also releases a manual acquisition with the same holder identity.

## Limits

A child inherits the lock descriptors. If it deliberately closes them and the wrapper is killed, the resource may be released while the child is still running.

When changing from private to shared mode, choose a new state directory; lease refuses to republish existing private history to the group.

Resource names are case-sensitive. Stop older lease processes before upgrading to this version; its resource files use a new encoding and do not coordinate with older versions.

## License

Apache-2.0. Copyright 2026 Dela Anthonio. See [LICENSE](LICENSE) and
[NOTICE](NOTICE).
