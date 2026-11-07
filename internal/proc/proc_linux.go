//go:build linux

package proc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// fingerprint reads the kernel's process start time in clock ticks since
// boot from /proc. The value only repeats for the same process incarnation.
func fingerprint(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// The comm field can contain spaces and parentheses; everything after
	// its closing parenthesis is fixed-position.
	stat := string(raw)
	idx := strings.LastIndexByte(stat, ')')
	if idx < 0 || idx+2 >= len(stat) {
		return ""
	}
	fields := strings.Fields(stat[idx+1:])
	// Field 22 overall (starttime) is field 20 in this remainder, which
	// starts at the overall field 3 (state).
	if len(fields) < 20 {
		return ""
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return ""
	}
	return "linux:" + strconv.FormatInt(ticks, 10)
}
