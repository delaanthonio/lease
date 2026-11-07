// Package identity derives who a caller is from its environment. A holder
// is named by task id or CI run id when the environment provides one, and
// falls back to user, pid, and command for interactive use.
package identity

import (
	"fmt"
	"os"
	"os/user"
	"strings"
)

// EnvTaskID and friends name the environment variables lease reads to
// label a holder.
const (
	EnvTaskID  = "FM_TASK_ID"
	EnvCIRunID = "GITHUB_RUN_ID"
	EnvCIJob   = "GITHUB_JOB"
)

// Identity describes a caller.
type Identity struct {
	TaskID  string
	CIRunID string
	CIJob   string
	User    string
}

// FromEnv builds the caller identity. Task id wins over CI markers, which
// win over the plain user fallback.
func FromEnv() Identity {
	return FromLookup(os.Getenv, CurrentUser)
}

// CurrentUser returns the name of the user running this process.
func CurrentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return ""
}

// FromLookup builds identity from an environment lookup, for tests.
func FromLookup(getenv func(string) string, lookupUser func() string) Identity {
	id := Identity{User: lookupUser()}
	if v := strings.TrimSpace(getenv(EnvTaskID)); v != "" {
		id.TaskID = v
	}
	if v := strings.TrimSpace(getenv(EnvCIRunID)); v != "" {
		id.CIRunID = v
	}
	if v := strings.TrimSpace(getenv(EnvCIJob)); v != "" {
		id.CIJob = v
	}
	return id
}

// Label returns the short display form: "task:<id>" or
// "ci:run/<run>/<job>" when the environment provides one, otherwise
// "user:<name>".
func (i Identity) Label() string {
	switch {
	case i.TaskID != "":
		return "task:" + i.TaskID
	case i.CIRunID != "":
		if i.CIJob != "" {
			return fmt.Sprintf("ci:run/%s/%s", i.CIRunID, i.CIJob)
		}
		return "ci:run/" + i.CIRunID
	case i.User != "":
		return "user:" + i.User
	default:
		return "unknown"
	}
}
