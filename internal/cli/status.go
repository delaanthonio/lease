package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/delaanthonio/lease/internal/lease"
	"github.com/delaanthonio/lease/internal/lock"
	"github.com/delaanthonio/lease/internal/paths"
	"github.com/delaanthonio/lease/internal/proc"
	"github.com/delaanthonio/lease/internal/store"
)

// StatusCommand implements lease status [NAME...] [--json].
func StatusCommand(argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("status", stderr)
	asJSON := fs.Bool("json", false, "emit stable, compact JSON for agents")
	configPath := fs.String("config", "", "path to the config file (default: $LEASE_CONFIG or the user config dir)")
	names, err := parseArgs(fs, argv)
	if err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
		return exitUsage
	}

	if err := lease.ValidateNames(names); err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintln(stderr, err)
		return exitUsage
	}
	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = paths.ConfigFile(os.LookupEnv)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
		return 1
	}

	st := store.NewFileStore(stateDir(cfg))
	if len(names) > 0 {
		if _, err := st.Events(); err != nil {
			// Output is best effort; preserve the command result.
			_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
			return 1
		}
	}

	// Named resources only, or every resource that exists on the machine.
	resources := names
	if len(resources) == 0 {
		existing, err := st.Resources()
		if err != nil {
			// Output is best effort; preserve the command result.
			_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
			return 1
		}
		resources = existing
		for name := range cfg.Resources {
			resources = append(resources, name)
		}
		sort.Strings(resources)
		resources = dedup(resources)
	}

	stateDir := stateDir(cfg)
	views, err := lease.Inspect(stateDir, st, cfg, resources)
	if err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
		return 1
	}

	if *asJSON {
		if err := RenderJSON(stdout, views); err != nil {
			// Output is best effort; preserve the command result.
			_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
			return 1
		}
		return 0
	}
	RenderTable(stdout, views, time.Now())
	return 0
}

// ConfigCommand implements lease config: show the resolved state
// directory, config file, and resource slot counts.
func ConfigCommand(argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("config", stderr)
	configPath := fs.String("config", "", "path to the config file (default: $LEASE_CONFIG or the user config dir)")
	if err := fs.Parse(argv); err != nil {
		return exitUsage
	}

	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = paths.ConfigFile(os.LookupEnv)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
		return 1
	}
	// Output is best effort; preserve the command result.
	_, _ = fmt.Fprintf(stdout, "state dir: %s\n", stateDir(cfg))
	if cfg.Path == "" {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintln(stdout, "config file: (none, defaults in effect)")
	} else {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stdout, "config file: %s\n", cfg.Path)
	}
	// Output is best effort; preserve the command result.
	_, _ = fmt.Fprintln(stdout, "resources:")
	names := make([]string, 0, len(cfg.Resources))
	for name := range cfg.Resources {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintln(stdout, "  (none configured; unknown names default to 1 exclusive slot)")
	}
	for _, name := range names {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stdout, "  %s: %d slot(s)\n", name, cfg.Slots(name))
	}
	return 0
}

func dedup(sorted []string) []string {
	out := sorted[:0]
	prev := ""
	for i, s := range sorted {
		if i == 0 || s != prev {
			out = append(out, s)
		}
		prev = s
	}
	return out
}

// ForceReleaseCommand implements lease force-release NAME --reason TEXT.
// It is the captain's tool for stuck locks: it clears holder records whose
// process is gone and logs the reason and previous holder to the
// history. It never kills a live process; a slot held by a
// running process stays held until that process exits.
func ForceReleaseCommand(argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("force-release", stderr)
	reason := fs.String("reason", "", "why the lock is being force-released (required, logged)")
	configPath := fs.String("config", "", "config file")
	names, err := parseArgs(fs, argv)
	if err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
		return exitUsage
	}
	if len(names) != 1 || *reason == "" {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintln(stderr, "usage: lease force-release NAME --reason TEXT")
		return exitUsage
	}
	name := names[0]
	if !store.NameValid(name) {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: invalid resource name %q\n", name)
		return exitUsage
	}

	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = paths.ConfigFile(os.LookupEnv)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	dir := stateDir(cfg)
	st := store.NewFileStore(dir)
	holders, err := st.Holders()
	if err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
		return 1
	}

	found := false
	for i := range holders {
		h := holders[i]
		if h.Resource != name {
			continue
		}
		found = true
		if proc.HolderIsStale(h.PID, h.ProcStart) == nil {
			// Output is best effort; preserve the command result.
			_, _ = fmt.Fprintf(stdout, "slot %d of %q is still held by a live process (%s); force-release never kills processes\n",
				h.Slot, name, lease.DescribeHolder(&h))
			continue
		}
		prev := lease.DescribeHolderForHistory(&h)
		cleared, err := clearObservedWith(st, dir, &h, func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := st.AppendEventContext(ctx, store.Event{
				Kind: store.EventForceRelease, Resource: name, Slot: h.Slot,
				Actor: identityFromEnv().Label(), Reason: *reason, Detail: "previous holder: " + prev,
			}, false); err != nil {
				return fmt.Errorf("record force-release reason: %w", err)
			}
			return nil
		})
		if err != nil {
			// Output is best effort; preserve the command result.
			_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
			return 1
		}
		if !cleared {
			// Output is best effort; preserve the command result.
			_, _ = fmt.Fprintf(stdout, "slot %d of %q is still locked; a descendant may still hold it\n", h.Slot, name)
			continue
		}
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stdout, "cleared stale holder of slot %d: %s\n", h.Slot, prev)
	}
	if !found {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stdout, "no holder records for %q\n", name)
	}
	return 0
}

// AcquireCommand implements lease acquire: take slots manually and print
// one release token for the acquisition. The slots are held by a keeper process
// until 'lease release TOKEN' signals it or it is killed; a killed keeper
// leaves the slot to the stale-holder takeover machinery.
func AcquireCommand(argv []string, stdout, stderr io.Writer) int {
	opts := RunOptions{}
	fs := newFlagSet("acquire", stderr)
	opts.register(fs)
	ownerPID := fs.Int("owner-pid", os.Getppid(), "process that owns the lease")
	names, err := parseArgs(fs, argv)
	if err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
		return exitUsage
	}
	if len(names) == 0 {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintln(stderr, "usage: lease acquire NAME [NAME...] [--slots N] [--wait DURATION | --no-wait] [--ttl DURATION]")
		return exitUsage
	}
	if opts.NoWait && opts.Wait > 0 {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintln(stderr, "lease acquire: --no-wait and --wait are mutually exclusive")
		return exitUsage
	}

	leaseOpts, err := opts.prepare(stderr)
	if err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
		if errors.Is(err, errInvalidOptions) {
			return exitUsage
		}
		return 1
	}

	if err := lease.ValidateNames(names); err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintln(stderr, err)
		return exitUsage
	}
	for _, name := range names {
		if opts.Slots > leaseOpts.Capacity(name) {
			// Output is best effort; preserve the command result.
			_, _ = fmt.Fprintf(stderr, "lease: resource %q has too few slots\n", name)
			return exitUsage
		}
	}
	ownerStart := proc.Fingerprint(*ownerPID)
	if ownerStart == "" {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintln(stderr, "lease: owner process is not available")
		return exitUsage
	}
	ttl := opts.TTL
	if ttl == 0 {
		ttl = leaseOpts.Config.ManualTTL
	}
	if ttl <= 0 {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintln(stderr, "lease: manual TTL must be positive")
		return exitUsage
	}
	token, err := newToken()
	if err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	tok, release, err := startKeeper(keeperFlags{StateDir: leaseOpts.StateDir, Names: names, Slots: opts.Slots, OwnerPID: *ownerPID, OwnerStart: ownerStart, Token: token, Config: opts.Config, Wait: opts.Wait, NoWait: opts.NoWait, TTL: ttl, Holder: holderIdentity{User: leaseOpts.Identity.User, TaskID: leaseOpts.Identity.TaskID, CIRunID: leaseOpts.Identity.CIRunID, CIJob: leaseOpts.Identity.CIJob, Command: "lease"}})
	if err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
		var busy *lease.BusyError
		if errors.As(err, &busy) {
			return lease.ExitBusy
		}
		return 1
	}
	if _, err := fmt.Fprintln(stdout, tok); err != nil {
		release()
		_, _ = fmt.Fprintf(stderr, "lease: write token: %s\n", err)
		return 1
	}
	return 0
}

// ReleaseCommand implements lease release TOKEN.
func ReleaseCommand(argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("release", stderr)
	configPath := fs.String("config", "", "config file")
	args, err := parseArgs(fs, argv)
	if err != nil {
		return exitUsage
	}
	if len(args) != 1 {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintln(stderr, "usage: lease release TOKEN")
		return exitUsage
	}
	if err := releaseToken(args[0], *configPath); err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
		return 1
	}
	return 0
}

func clearObserved(st store.Store, dir string, observed *store.Holder) (bool, error) {
	return clearObservedWith(st, dir, observed, nil)
}

func clearObservedWith(st store.Store, dir string, observed *store.Holder, beforeClear func() error) (bool, error) {
	f, err := lock.OpenSlot(dir, observed.Resource, observed.Slot)
	if err != nil {
		return false, err
	}
	// Cleanup is best effort; preserve the existing result.
	defer func() { _ = f.Close() }()
	ok, err := lock.TryLock(f)
	if err != nil || !ok {
		return false, err
	}
	// Cleanup is best effort; preserve the existing result.
	defer func() { _ = lock.Unlock(f) }()
	current, err := st.Holder(observed.Resource, observed.Slot)
	if err != nil || current == nil || *current != *observed {
		return false, err
	}
	if beforeClear != nil {
		if err := beforeClear(); err != nil {
			return false, err
		}
	}
	return true, st.ClearHolder(observed.Resource, observed.Slot)
}
