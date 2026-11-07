package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/delaanthonio/lease/internal/lease"
	"github.com/delaanthonio/lease/internal/paths"
	"github.com/delaanthonio/lease/internal/store"
)

// exitUsage reports a command-line usage problem.
const exitUsage = 2

// newFlagSet builds a flag set that reports usage problems to stderr and
// returns instead of exiting.
func newFlagSet(name string, stdout, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stdout, "Usage of lease %s:\n", name)
		fs.SetOutput(stdout)
		fs.PrintDefaults()
		fs.SetOutput(stderr)
	}
	return fs
}

// splitCommand separates everything before the first "--" (flags and
// names) from the command to run.
func splitCommand(args []string) (pre, cmd []string, err error) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:], nil
		}
	}
	return nil, nil, errors.New("missing \"--\" followed by the command to run")
}

// parseArgs parses flags and positional arguments in any order, so both
// "status --json db" and "status db --json" work. It returns the
// positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		left := fs.Args()
		if len(left) == 0 {
			return positional, nil
		}
		if strings.HasPrefix(left[0], "-") {
			return nil, fmt.Errorf("unknown flag %s", left[0])
		}
		positional = append(positional, left[0])
		rest = left[1:]
	}
}

// RunOptions are the flags shared by run and acquire.
type RunOptions struct {
	Slots         int
	Wait          time.Duration
	NoWait        bool
	TTL           time.Duration
	Config        string
	RecordCommand bool
}

func (r *RunOptions) register(fs *flag.FlagSet) {
	fs.BoolVar(&r.RecordCommand, "record-command", false, "record the full command line")
	fs.IntVar(&r.Slots, "slots", 1, "number of slots to take of each resource")
	fs.DurationVar(&r.Wait, "wait", 0, "how long to wait for busy slots, e.g. 5m; default waits forever")
	fs.BoolVar(&r.NoWait, "no-wait", false, "fail immediately when a slot is busy")
	fs.DurationVar(&r.TTL, "ttl", 0, "release a holder that is gone and held longer than this, e.g. 30m")
	fs.StringVar(&r.Config, "config", "", "path to the config file (default: $LEASE_CONFIG or the user config dir)")
}

var errInvalidOptions = errors.New("invalid acquisition options")

// prepare resolves the shared machinery behind run and acquire.
func (r *RunOptions) prepare(stderr io.Writer) (*lease.Options, error) {
	if r.Slots <= 0 {
		return nil, fmt.Errorf("%w: --slots must be positive", errInvalidOptions)
	}
	if r.Wait < 0 {
		return nil, fmt.Errorf("%w: --wait must not be negative", errInvalidOptions)
	}
	cfgPath := r.Config
	if cfgPath == "" {
		cfgPath = paths.ConfigFile(os.LookupEnv)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	if cfg.Shared && (r.RecordCommand || cfg.RecordCommand) {
		return nil, fmt.Errorf("full command recording is unavailable in shared mode")
	}
	if r.NoWait {
		r.Wait = time.Nanosecond
	}
	opts := &lease.Options{
		StateDir:  stateDir(cfg),
		Store:     store.NewFileStore(stateDir(cfg)),
		Config:    cfg,
		Identity:  identityFromEnv(),
		Requested: r.Slots,
		Wait:      r.Wait,
		TTL:       r.TTL,
		Note: func(format string, args ...any) {
			// Output is best effort; preserve the command result.
			_, _ = fmt.Fprintf(stderr, "lease: "+format+"\n", args...)
		},
	}
	return opts, nil
}

// RunCommand implements lease run: acquire every named resource in sorted
// order, run the command with inherited stdio, always release, and return
// the command's exit code.
func RunCommand(argv []string, stdout, stderr io.Writer) int {
	opts0 := RunOptions{}
	fs := newFlagSet("run", stdout, stderr)
	opts0.register(fs)
	pre, cmdArgs, splitErr := splitCommand(argv)
	if splitErr != nil {
		pre = argv
	}

	names, err := parseArgs(fs, pre)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease run: %s\n", err)
		return exitUsage
	}
	if splitErr != nil {
		_, _ = fmt.Fprintf(stderr, "lease run: %s\n", splitErr)
		return exitUsage
	}
	if len(names) == 0 || len(cmdArgs) == 0 {
		// Usage output is best effort.
		_, _ = fmt.Fprintln(stderr, "usage: lease run NAME [NAME...] [--slots N] [--wait DURATION | --no-wait] [--ttl DURATION] -- CMD ARGS")
		return exitUsage
	}
	if opts0.NoWait && opts0.Wait > time.Nanosecond {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintln(stderr, `lease run: --no-wait and --wait are mutually exclusive`)
		return exitUsage
	}

	opts, err := opts0.prepare(stderr)
	if err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
		if errors.Is(err, errInvalidOptions) {
			return exitUsage
		}
		return 1
	}
	opts.Command = filepath.Base(cmdArgs[0])
	if opts0.RecordCommand || opts.Config.RecordCommand {
		opts.Command = strings.Join(cmdArgs, " ")
	}

	// Watch for termination signals from before the lock is held until the
	// child exits, so the locks are always released.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	opts.Context = ctx

	acq, err := opts.Acquire(names)
	if err != nil {
		select {
		case sig := <-sigCh:
			// Output is best effort; preserve the command result.
			_, _ = fmt.Fprintf(stderr, "lease: interrupted by %s\n", sig)
			return 128 + int(sig.(syscall.Signal))
		default:
		}
		var busy *lease.BusyError
		if errors.As(err, &busy) {
			// Output is best effort; preserve the command result.
			_, _ = fmt.Fprintf(stderr, "lease: %s\n", busy)
			// Output is best effort; preserve the command result.
			_, _ = fmt.Fprintf(stderr, "lease: retry with --wait 5m, or inspect with 'lease status %s'\n", busy.Resource)
			return lease.ExitBusy
		}
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
		return 1
	}
	defer acq.Release()

	// A signal that arrived while waiting for slots releases and exits.
	select {
	case sig := <-sigCh:
		return 128 + int(sig.(syscall.Signal))
	default:
	}

	child := exec.Command(cmdArgs[0], cmdArgs[1:]...) //nolint:gosec // the command is the caller's choice, run verbatim
	child.ExtraFiles = acq.Files()
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: %s\n", err)
		return 1
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	for {
		select {
		case sig := <-sigCh:
			// Forward the signal to the child; its own exit code will
			// reflect it, and we release when it exits.
			_ = child.Process.Signal(sig)
		case err := <-done:
			return childExitCode(err)
		}
	}
}

// childExitCode maps a child Wait error to the process exit code.
func childExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exitErr.ExitCode()
	}
	return 1
}
