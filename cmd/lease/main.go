// Command lease gives out named locks for scarce machine resources so
// workers, skills, and CI jobs on one machine can share them without
// stepping on each other.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/delaanthonio/lease/internal/cli"
	"github.com/delaanthonio/lease/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	stdout, stderr := os.Stdout, os.Stderr
	switch args[0] {
	case "run":
		return cli.RunCommand(args[1:], stdout, stderr)
	case "status":
		return cli.StatusCommand(args[1:], stdout, stderr)
	case "acquire":
		return cli.AcquireCommand(args[1:], stdout, stderr)
	case "release":
		return cli.ReleaseCommand(args[1:], stdout, stderr)
	case "force-release":
		return cli.ForceReleaseCommand(args[1:], stdout, stderr)
	case "config":
		return cli.ConfigCommand(args[1:], stdout, stderr)
	case "version":
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stdout, "lease %s\n", version.Version)
		return 0
	case "_keeper":
		// Hidden: the backing process behind 'lease acquire'.
		return cli.KeeperCommand(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		// Output is best effort; preserve the command result.
		_, _ = fmt.Fprintf(stderr, "lease: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	// Usage output is best effort.
	_, _ = fmt.Fprint(w, `lease - named locks for scarce machine resources

usage:
  lease run NAME [NAME...] [--slots N] [--wait DURATION | --no-wait] [--ttl DURATION] -- CMD ARGS
  lease status [NAME...] [--json]
  lease acquire NAME [NAME...] [--slots N] [--wait DURATION | --no-wait] [--ttl DURATION]
  lease release TOKEN
  lease force-release NAME --reason TEXT
  lease config
  lease version

run the same command with --help for its flags.
`)
}
