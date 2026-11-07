# lease

Named locks for scarce machine resources: an iOS Simulator device, dev-server
ports, a local Postgres, Xcode builds, a self-hosted runner. When workers,
skills, and CI jobs share one machine, `lease` keeps them from stepping on
each other.

Locks are kernel file locks (`flock(2)`) in a machine-wide directory. There
is no daemon: a lock is held by a process, and the kernel releases it the
moment that process dies, even after a crash or a `kill -9`. Holder records
detect stale holders and take over automatically.

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
Binaries are built for macOS (arm64) and Linux (amd64, arm64). Or build from
source with Go 1.25 or newer:

```sh
go install github.com/delaanthonio/lease/cmd/lease@latest
```

## Quick start

Wrap the command that uses the scarce resource:

```sh
# One exclusive lock; a second caller waits (default) or fails with --no-wait.
lease run ios-simulator -- xcrun simctl boot "iPhone 16"

# Wait at most five minutes, then give up.
lease run db --wait 5m -- ./scripts/seed-db

# Counted slots: configure two slots, then each caller takes one.
mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/lease"
export LEASE_CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/lease/config.toml"
printf '[resources.build]\nslots = 2\n' > "$LEASE_CONFIG"
lease run build -- xcodebuild -scheme App build
```

`lease run` acquires every named lock in a fixed sorted order, runs the
command with inherited stdio, always releases the locks when the command
exits (including on signals), and returns the command's exit code.

Inspect what is held:

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

## Adopting it in a mise task

```toml
[tasks.test-sim]
run = "lease run ios-simulator -- sh -c \"xcrun simctl boot 'iPhone 16' && ./scripts/ui-tests\""

[tasks.dev-server]
run = "lease run dev-port-3000 --wait 5m -- ./scripts/dev-server"
```

### Cooperative enforcement

A lock only works when every user of the resource goes through `lease`. The
kernel cannot stop a script from calling `xcrun simctl boot` directly. Make
the wrapper the norm with a lint-friendly check that fails when a
scarce-resource command appears outside `lease run`:

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
| `lease force-release NAME --reason TEXT` | clear a stuck lock; captain-word only |
| `lease config` | show resolved resources and slot counts |

### Manual acquire and release

```sh
token=$(lease acquire dev-port-3000)   # one slot, held by a small keeper process
...use the resource...
lease release "$token"
```

`lease acquire` prints one token for the complete set of resources and slots. A small keeper process
holds the kernel lock until `lease release TOKEN` signals it or it is
killed; a killed keeper leaves the slot to the stale-holder takeover
machinery. If a script acquires and dies before releasing, the lock does
not leak.

### Force release

`lease force-release NAME --reason TEXT` is the captain's tool for stuck
locks. It clears holder records whose process is gone and writes the
reason and the previous holder to the append-only history. It never kills
a live process: a slot held by a running process stays held until that
process exits. Restrict it to whoever runs the machine (the "captain
word"); everyday automation should never need it.

## Slots and configuration

An optional TOML file declares how many callers a resource admits:

```toml
[resources.ios-simulator]
slots = 1

[resources.dev-port-3000]
slots = 1

[resources.build]
slots = 3
```

The file lives at `$LEASE_CONFIG`, or `lease config.toml` under the user
config directory (`$XDG_CONFIG_HOME/lease/config.toml`, or
`~/.config/lease/config.toml` on Linux,
`~/Library/Application Support/lease/config.toml` on macOS). Resources
absent from the file default to one exclusive slot. `lease config` shows
what is resolved.

## Holder identity

A holder is named from the environment: `FM_TASK_ID` for agent tasks,
`GITHUB_RUN_ID` and `GITHUB_JOB` for CI, otherwise the user, pid, and
command. The status view and busy messages always say who holds a resource
and since when.

## Exit codes

| code | meaning |
| --- | --- |
| 0 | success (or the command exited 0) |
| 1 | lease itself failed |
| 2 | usage error |
| 75 | the resource is busy or the wait elapsed (EX_TEMPFAIL) |
| n | the command's own exit code, `128+n` when it died from signal n |

A busy failure names the current holder and how long it has held the lock:

```text
lease: resource "db" slot 1 is held by user:alice pid 4242 seed-db since 2026-11-07T09:12:00Z
```

## Where state lives

Everything is in one machine-wide directory, outside any worktree:
`$LEASE_STATE_DIR`, else `lease` under the user state dir
(`$XDG_STATE_HOME/lease` or `~/.local/state/lease` on Linux,
`~/Library/Application Support/lease` on macOS). Inside:

- `locks/<resource>/slot-N.lock` - the kernel lock files; holding a slot
  means holding an exclusive `flock` on one of these
- `holders/`, `waiters/` - small JSON records: who holds what, who waits
- `events.ndjson` - append-only history, including every force-release

Stale holders (pid gone, or the pid was reused by a different process) are
detected and taken over automatically, with a note on stderr and an entry
in the history. `--ttl` releases a hung holder only when it is also
unresponsive: a record older than the TTL whose process is gone is evicted,
while a live process keeps its lock no matter how long it has held it.

All metadata goes through an internal `Store` interface with the
file-backed implementation as the only one today; a database-backed
implementation (for history and stats) can replace it later without
changing the commands.

## Scope

lease is deliberately small: no scheduling, no priorities, no load-average
awareness, no killing of processes. It coordinates; it does not decide.

## License

Apache-2.0. Copyright 2026 Dela Anthonio. See [LICENSE](LICENSE) and
[NOTICE](NOTICE).


### Coordination and privacy

Workers, skills, and CI must run as the same user, or use the same `LEASE_DIR` and a common group. Set `dir` in the config as an alternative to `LEASE_DIR`; environment overrides take precedence. For a group-owned directory, set `shared = true` in every participant's config. New directories use mode 0770 and coordination files 0660; the default is 0700 and 0600. History is private (0600) by default and group-readable and writable (0660) in shared mode. The shared directory must be owned by the common group and have the setgid bit set so new files inherit that group.

Commands record only the executable basename and holder identity by default. Use `--record-command` or `record_command = true` to include arguments in private mode; shared mode refuses full command recording. History retains at most 1,000 events and 30 days, whichever is smaller, enforced on append, history reads, and status. History is best-effort diagnostics, not an audit trail; events can be dropped under contention. Force-release records its reason before clearing a stale holder. Command recording is off by default; files outside lease are not touched by retention cleanup.

Manual acquisitions use the calling parent as owner; `--owner-pid` selects another owner. A keeper releases when its owner exits or its TTL expires. The default TTL is 30 minutes, configurable with `manual_ttl = "30m"` or `--ttl`. A single token represents the complete acquisition; `lease release NAME` also releases a manual acquisition with the same holder identity.

### Limits

A child inherits the lock descriptors. If it deliberately closes them and the wrapper is killed, the resource may be released while the child is still running.

When changing from private to shared mode, choose a new state directory; lease refuses to republish existing private history to the group.

Resource names are case-sensitive. Stop older lease processes before upgrading to this version; its resource files use a new encoding and do not coordinate with older versions.
