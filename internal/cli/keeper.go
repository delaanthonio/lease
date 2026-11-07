package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/delaanthonio/lease/internal/identity"
	"github.com/delaanthonio/lease/internal/lease"
	"github.com/delaanthonio/lease/internal/paths"
	"github.com/delaanthonio/lease/internal/proc"
	"github.com/delaanthonio/lease/internal/store"
)

// keeperFlags carries everything a keeper process needs to hold one slot:
// where the state lives, the release token, and the identity of the human
// or agent that acquired it. The keeper itself picks whichever slot of the
// resource is free first.
type keeperFlags struct {
	StateDir    string
	Names       []string
	Slots       int
	OwnerPID    int
	OwnerStart  string
	ClientPID   int
	ClientStart string
	Token       string
	Config      string
	Wait        time.Duration
	NoWait      bool
	TTL         time.Duration
	Holder      holderIdentity
}

// holderIdentity is the acquirer identity passed to the keeper, matching
// the fields of a holder record.
type holderIdentity struct {
	User    string `json:"user"`
	TaskID  string `json:"task_id,omitempty"`
	CIRunID string `json:"ci_run_id,omitempty"`
	CIJob   string `json:"ci_job,omitempty"`
	Command string `json:"command,omitempty"`
}

// startKeeper spawns one keeper process for a slot and waits for it to
// report the acquisition result on its stdout pipe. The returned token is
// what 'lease release' later accepts; the returned release function stops
// the keeper (freeing the slot) when the caller gives up on a partial
// acquisition.
func startKeeper(kf keeperFlags) (token string, release func(), err error) {
	holder, err := json.Marshal(kf.Holder)
	if err != nil {
		return "", nil, err
	}
	namesJSON, err := json.Marshal(kf.Names)
	if err != nil {
		return "", nil, err
	}
	argv := []string{
		os.Args[0], "_keeper",
		"--state-dir", kf.StateDir,
		"--names-json", string(namesJSON),
		"--slots", strconv.Itoa(kf.Slots),
		"--owner-pid", strconv.Itoa(kf.OwnerPID),
		"--owner-start", kf.OwnerStart,
		"--client-pid", strconv.Itoa(os.Getpid()),
		"--client-start", proc.Fingerprint(os.Getpid()),
		"--token", kf.Token,
		"--holder-json", string(holder),
	}
	if kf.Config != "" {
		argv = append(argv, "--config", kf.Config)
	}
	if kf.Wait > 0 {
		argv = append(argv, "--wait", kf.Wait.String())
	}
	if kf.NoWait {
		argv = append(argv, "--no-wait")
	}
	if kf.TTL > 0 {
		argv = append(argv, "--ttl", kf.TTL.String())
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	// The keeper reports its result on the stdout pipe and keeps nothing
	// else open: a null stderr, and the parent's own stdio marked
	// close-on-exec so a shell capturing the tokens is not held open by
	// the long-lived keeper after 'lease acquire' exits.
	cmd.Stderr = nil
	markStdioCloseOnExec()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}
	keeperDone := false
	defer func() {
		// If the keeper is still alive at this point the parent is giving
		// up on it (a failed acquisition); release its slot.
		if !keeperDone {
			stop()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		return "", nil, fmt.Errorf("keeper for %q did not report: %w", kf.Names, err)
	}
	if strings.HasPrefix(line, "busy ") {
		var busy lease.BusyError
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "busy "))), &busy); err != nil {
			return "", nil, fmt.Errorf("invalid keeper busy response: %w", err)
		}
		return "", nil, &busy
	}
	if strings.HasPrefix(line, "err ") {
		return "", nil, fmt.Errorf("keeper for %q failed: %s", kf.Names, strings.TrimSpace(strings.TrimPrefix(line, "err ")))
	}
	if !strings.HasPrefix(line, "ok ") {
		return "", nil, fmt.Errorf("keeper for %q failed: %s", kf.Names, strings.TrimSpace(line))
	}
	keeperDone = true
	token = strings.TrimSpace(strings.TrimPrefix(line, "ok "))
	return token, stop, nil
}

// KeeperCommand implements the hidden keeper subcommand: acquire one slot,
// write the holder record with the release token, and hold until a
// release signal or death. The kernel frees the flock if the keeper is
// killed, and the stale-holder machinery takes over from there.
func KeeperCommand(argv []string, stdout, stderr io.Writer) int {
	kf := keeperFlags{}
	fs := newFlagSet("_keeper", stderr)
	fs.StringVar(&kf.StateDir, "state-dir", "", "")
	namesJSON := fs.String("names-json", "", "")
	fs.IntVar(&kf.Slots, "slots", 1, "")
	fs.IntVar(&kf.OwnerPID, "owner-pid", 0, "")
	fs.StringVar(&kf.OwnerStart, "owner-start", "", "")
	fs.IntVar(&kf.ClientPID, "client-pid", 0, "")
	fs.StringVar(&kf.ClientStart, "client-start", "", "")
	fs.StringVar(&kf.Token, "token", "", "")
	fs.StringVar(&kf.Config, "config", "", "")
	fs.DurationVar(&kf.Wait, "wait", 0, "")
	fs.BoolVar(&kf.NoWait, "no-wait", false, "")
	fs.DurationVar(&kf.TTL, "ttl", 0, "")
	holderJSON := fs.String("holder-json", "", "")
	if err := fs.Parse(argv); err != nil {
		return exitUsage
	}
	if err := json.Unmarshal([]byte(*namesJSON), &kf.Names); err != nil || len(kf.Names) == 0 || kf.StateDir == "" || kf.Slots < 1 || kf.TTL <= 0 || kf.OwnerStart == "" || proc.HolderIsStale(kf.OwnerPID, kf.OwnerStart) != nil {
		return exitUsage
	}
	if err := json.Unmarshal([]byte(*holderJSON), &kf.Holder); err != nil {
		fmt.Fprintf(stderr, "lease: bad holder identity: %s\n", err)
		return exitUsage
	}
	if kf.NoWait {
		kf.Wait = time.Nanosecond
	}

	cfgPath := kf.Config
	if cfgPath == "" {
		cfgPath = paths.ConfigFile(os.LookupEnv)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(stdout, "err %s\n", err)
		return 1
	}

	opts := &lease.Options{
		StateDir:  kf.StateDir,
		Store:     store.NewFileStore(kf.StateDir),
		Config:    cfg,
		Identity:  identity.Identity{User: kf.Holder.User, TaskID: kf.Holder.TaskID, CIRunID: kf.Holder.CIRunID, CIJob: kf.Holder.CIJob},
		Requested: kf.Slots,
		Wait:      kf.Wait,
		TTL:       kf.TTL,
		Token:     kf.Token,
		Command:   kf.Holder.Command,
		Note: func(format string, args ...any) {
			fmt.Fprintf(stderr, "lease: "+format+"\n", args...)
		},
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	acquiring := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				select {
				case <-acquiring:
				default:
					if kf.ClientStart == "" || proc.HolderIsStale(kf.ClientPID, kf.ClientStart) != nil {
						cancel()
						return
					}
				}
				if proc.HolderIsStale(kf.OwnerPID, kf.OwnerStart) != nil {
					cancel()
					return
				}
			}
		}
	}()
	opts.Context = ctx
	acq, err := opts.Acquire(kf.Names)
	if err != nil {
		var busy *lease.BusyError
		if errors.As(err, &busy) {
			fmt.Fprint(stdout, "busy ")
			_ = json.NewEncoder(stdout).Encode(busy)
			return lease.ExitBusy
		}
		// stderr is /dev/null for a keeper; report over the protocol.
		fmt.Fprintf(stdout, "err %s\n", err)
		return 1
	}

	defer acq.Release()
	if ctx.Err() != nil {
		return 1
	}
	close(acquiring)
	fmt.Fprintf(stdout, "ok %s\n", kf.Token)
	timer := time.NewTimer(kf.TTL)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}

	return 0
}

// markStdioCloseOnExec sets the close-on-exec flag on this process's own
// stdio so spawned keepers never inherit the caller's pipes. Without it, a
// keeper holding a slot keeps the parent's stdout open, which hangs any
// shell capture like tokens=$(lease acquire db) until the slot is
// released.
func markStdioCloseOnExec() {
	for _, f := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		// fcntl(fd, F_SETFD, FD_CLOEXEC); the constants are 2 and 1 on
		// every supported platform.
		_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), 2, 1)
	}
}

// releaseToken releases the slot held for a token: it finds the holder
// record, signals its keeper, and waits for the record to clear.
func releaseToken(token, cfgPath string) error {
	if cfgPath == "" {
		cfgPath = paths.ConfigFile(os.LookupEnv)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	dir := stateDir(cfg)
	st := store.NewFileStore(dir)
	holders, err := st.Holders()
	if err != nil {
		return err
	}
	var selected *store.Holder
	id := identityFromEnv()
	for i := range holders {
		h := &holders[i]
		matchName := h.Resource == token && h.User == id.User && h.TaskID == id.TaskID && h.CIRunID == id.CIRunID && h.CIJob == id.CIJob
		if h.Token != "" && (h.Token == token || matchName) {
			selected = h
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("unknown token or resource %q", token)
	}
	token = selected.Token
	if selected.ProcStart != "" && proc.HolderIsStale(selected.PID, selected.ProcStart) == nil {
		if err := syscall.Kill(selected.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		remaining := false
		for _, h := range holders {
			if h.Token != token {
				continue
			}
			cur, err := st.Holder(h.Resource, h.Slot)
			if err != nil {
				return err
			}
			if cur == nil || *cur != h {
				continue
			}
			remaining = true
			if proc.HolderIsStale(h.PID, h.ProcStart) != nil {
				cleared, err := clearObserved(st, dir, &h)
				if err != nil {
					return err
				}
				if cleared {
					if err := store.AppendEventContext(context.Background(), st, store.Event{Kind: store.EventRelease, Resource: h.Resource, Slot: h.Slot, Reason: "keeper already gone; stale record cleared"}, true); err != nil && !errors.Is(err, store.ErrHistoryBusy) {
						return err
					}
				}
			}
		}
		if !remaining {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out releasing token %q", token)
}

// newToken returns a fresh release token.
func newToken() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "l-" + hex.EncodeToString(buf[:]), nil
}
