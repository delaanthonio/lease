//go:build darwin || (!linux && !plan9 && !windows)

package proc

import (
	"os/exec"
	"strconv"
	"strings"
)

// fingerprint shells out to ps, whose lstart column gives the wall-clock
// start time of the process. Two incarnations of the same PID start at
// different times, so the value identifies the process incarnation. The
// invocation pins the time zone and locale so the output is identical
// across caller environments.
func fingerprint(pid int) string {
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = []string{"TZ=UTC", "LC_ALL=C"}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return "ps:" + strings.TrimSpace(string(out))
}
