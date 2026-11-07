//go:build darwin || (!linux && !plan9 && !windows)

package proc

import (
	"os/exec"
	"strconv"
	"strings"
)

// fingerprint shells out to ps, whose lstart column gives the wall-clock
// start time of the process. Two incarnations of the same PID start at
// different times, so the value identifies the process incarnation.
func fingerprint(pid int) string {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return "ps:" + strings.TrimSpace(string(out))
}
