package status

import (
	"strings"
	"testing"
	"time"

	"github.com/praveen9161/gpufit/agent/internal/pools"
)

func TestRender(t *testing.T) {
	now := time.Now()
	s := Status{Mode: Observe, Updated: now.Add(-7 * time.Second), Nodes: map[string]Node{
		"node-a": {Pool: pools.Training, Scores: pools.Scores{"gpu-0": {"training": 100, "finetune": 100, "inference": 100}}},
		"node-b": {Pool: pools.Inference, Reason: "gpu-0 training 50 < 80", Repair: map[string]string{"gpu-1": "inference 0 < 50"},
			Scores: pools.Scores{"gpu-0": {"training": 50, "finetune": 78, "inference": 76}, "gpu-1": {"training": 0, "finetune": 0, "inference": 0}}},
	}}
	var b strings.Builder
	s.Render(&b, now)
	out := b.String()
	for _, want := range []string{
		"mode: observe (reporting only",
		"updated 7s ago",
		"inference (would apply)",
		"unfit for training",
		"needs repair: inference 0 < 50",
		"1 of 2 nodes out of the training pool (in observe mode, nothing was changed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Enforce mode: no "(would apply)".
	s.Mode = Enforce
	for n, v := range s.Nodes {
		v.Applied = true
		s.Nodes[n] = v
	}
	b.Reset()
	s.Render(&b, now)
	if strings.Contains(b.String(), "would apply") {
		t.Errorf("enforce output says would apply:\n%s", b.String())
	}
}
