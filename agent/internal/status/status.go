// Package status is what gpufit-controller records about every node (ConfigMap
// gpufit-status, key status.json) and how `kubectl gpufit status` shows it.
package status

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/praveen9161/gpufit/agent/internal/pools"
	"github.com/praveen9161/gpufit/agent/internal/report"
)

const (
	ConfigMapKey = "status.json"
	Observe      = "observe"
	Enforce      = "enforce"
)

// Status is the controller's view of the cluster.
type Status struct {
	Mode    string          `json:"mode"`
	Updated time.Time       `json:"updated"`
	Nodes   map[string]Node `json:"nodes"`
	// Estimated cost of degraded GPUs to training jobs, since Report.Since.
	Report *report.Summary `json:"report,omitempty"`
}

type Node struct {
	Pool    string            `json:"pool"`
	Reason  string            `json:"reason"`
	Repair  map[string]string `json:"repair,omitempty"` // GPU -> reason
	Applied bool              `json:"applied"`          // false in observe mode
	Scores  pools.Scores      `json:"scores"`
}

// Render prints a table, one row per GPU:
//
//	NODE    POOL                      GPU    TRAIN  FTUNE  INFER  NOTE
//	node-b  inference (would apply)   gpu-0  50     78     76     gpu-0 training 50 < 80
func (s Status) Render(w io.Writer, now time.Time) {
	switch s.Mode {
	case Observe:
		fmt.Fprintf(w, "GPUFit mode: observe (reporting only; scheduling is not changed)")
	default:
		fmt.Fprintf(w, "GPUFit mode: %s", s.Mode)
	}
	fmt.Fprintf(w, "   updated %s ago\n\n", now.Sub(s.Updated).Round(time.Second))

	names := make([]string, 0, len(s.Nodes))
	for n := range s.Nodes {
		names = append(names, n)
	}
	sort.Strings(names)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tPOOL\tGPU\tTRAIN\tFTUNE\tINFER\tNOTE")
	degraded := 0
	for _, name := range names {
		n := s.Nodes[name]
		pool := n.Pool
		if n.Pool != pools.Training {
			degraded++
			if !n.Applied {
				pool += " (would apply)"
			}
		}
		gpus := make([]string, 0, len(n.Scores))
		for g := range n.Scores {
			gpus = append(gpus, g)
		}
		sort.Strings(gpus)
		for i, g := range gpus {
			node, p := name, pool
			if i > 0 {
				node, p = "", ""
			}
			var notes []string
			if strings.Contains(n.Reason, g+" ") && n.Pool != pools.Training {
				notes = append(notes, "unfit for training")
			}
			if r, ok := n.Repair[g]; ok {
				notes = append(notes, "needs repair: "+r)
			}
			sc := n.Scores[g]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n", node, p, g, sc["training"], sc["finetune"], sc["inference"], strings.Join(notes, "; "))
		}
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d of %d nodes out of the training pool", degraded, len(names))
	if s.Mode == Observe && degraded > 0 {
		fmt.Fprint(w, " (in observe mode, nothing was changed; set mode=enforce to act)")
	}
	fmt.Fprintln(w)
}
