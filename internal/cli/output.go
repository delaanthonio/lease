package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/delaanthonio/lease/internal/lease"
)

// jsonResourceView is the stable, compact JSON shape of one resource in
// lease status --json. Field set and order are a public contract for
// agents; additions are append-only.
type jsonResourceView struct {
	Resource string         `json:"resource"`
	Total    int            `json:"total"`
	Used     int            `json:"used"`
	Waiting  int            `json:"waiting"`
	Holders  []jsonHolderVM `json:"holders"`
}

type jsonHolderVM struct {
	Slot    int    `json:"slot"`
	Holder  string `json:"holder,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Command string `json:"command,omitempty"`
	Since   string `json:"since,omitempty"` // RFC 3339
	Stale   bool   `json:"stale,omitempty"`
}

// RenderJSON writes the resource views as compact JSON, one resource per
// line inside a top-level array.
func RenderJSON(w io.Writer, views []lease.ResourceView) error {
	out := make([]jsonResourceView, 0, len(views))
	for _, v := range views {
		jv := jsonResourceView{
			Resource: v.Resource,
			Total:    v.Total,
			Used:     v.Used,
			Waiting:  v.Waiting,
			Holders:  []jsonHolderVM{},
		}
		for slot := 1; slot <= v.Total; slot++ {
			h := v.Holders[slot-1]
			if h == nil {
				if len(v.SlotUsed) >= slot && v.SlotUsed[slot-1] {
					jv.Holders = append(jv.Holders, jsonHolderVM{Slot: slot, Holder: "unknown"})
				}
				continue
			}
			jv.Holders = append(jv.Holders, jsonHolderVM{
				Slot:    h.Slot,
				Holder:  lease.DescribeHolder(h),
				PID:     h.PID,
				Command: h.Command,
				Since:   h.AcquiredAt.UTC().Format(time.RFC3339),
			})
		}
		out = append(out, jv)
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}

// RenderTable writes the human-readable status table.
func RenderTable(w io.Writer, views []lease.ResourceView, now time.Time) {
	rows := make([][]string, 0, len(views))
	for _, v := range views {
		if v.Used == 0 {
			// A resource with nothing held still shows its capacity.
			rows = append(rows, []string{v.Resource, "-", "-", fmt.Sprintf("0/%d", v.Total), fmt.Sprint(v.Waiting)})
			continue
		}
		for i, h := range v.Holders {
			if h == nil {
				if len(v.SlotUsed) > i && v.SlotUsed[i] {
					rows = append(rows, []string{v.Resource, "unknown", "-", fmt.Sprintf("%d/%d", v.Used, v.Total), fmt.Sprint(v.Waiting)})
				}
				continue
			}
			holder := lease.DescribeHolder(h)
			rows = append(rows, []string{
				v.Resource,
				holder,
				humanizeSince(h.AcquiredAt, now),
				fmt.Sprintf("%d/%d", v.Used, v.Total),
				fmt.Sprint(v.Waiting),
			})
		}
	}
	if len(rows) == 0 {
		// Table output is best effort.
		_, _ = fmt.Fprintln(w, "no resources to show")
		return
	}
	header := []string{"RESOURCE", "HOLDER", "SINCE", "SLOTS", "WAITING"}
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	printRow := func(cells []string) {
		var b strings.Builder
		for i, cell := range cells {
			b.WriteString(cell)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-len(cell)+2))
			}
		}
		// Table output is best effort.
		_, _ = fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	printRow(header)
	sep := make([]string, len(header))
	for i, w := range widths {
		sep[i] = strings.Repeat("-", w)
	}
	printRow(sep)
	for _, row := range rows {
		printRow(row)
	}
}

// humanizeSince renders an age compactly, e.g. "5m" or "2h13m".
func humanizeSince(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		days := int(d.Hours()) / 24
		return fmt.Sprintf("%dd%dh", days, int(d.Hours())%24)
	}
}
