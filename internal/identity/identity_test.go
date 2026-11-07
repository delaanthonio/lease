package identity

import (
	"testing"
)

func TestFromLookupPrecedence(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"task id wins", map[string]string{"FM_TASK_ID": "t1", "GITHUB_RUN_ID": "9", "GITHUB_JOB": "build"}, "task:t1"},
		{"ci run and job", map[string]string{"GITHUB_RUN_ID": "9", "GITHUB_JOB": "build"}, "ci:run/9/build"},
		{"ci run only", map[string]string{"GITHUB_RUN_ID": "9"}, "ci:run/9"},
		{"user fallback", map[string]string{}, "user:alice"},
		{"blank values fall through", map[string]string{"FM_TASK_ID": "  "}, "user:alice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := FromLookup(func(k string) string { return c.env[k] }, func() string { return "alice" })
			if got := id.Label(); got != c.want {
				t.Errorf("Label() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestLabelWithoutUser(t *testing.T) {
	id := FromLookup(func(string) string { return "" }, func() string { return "" })
	if got := id.Label(); got != "unknown" {
		t.Errorf("Label() = %q, want unknown", got)
	}
}
