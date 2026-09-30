// Package pools decides, from per-GPU scores, which pool each node belongs to
// (training or inference) and which GPUs need repair, with hysteresis so noisy
// scores don't make nodes flap. Design: docs/design/node-pools.md.
//
// It's pure logic (scores + time in, decisions out) so it can be tested
// without a cluster; cmd/gpufit-controller applies the decisions.
package pools

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	Training  = "training"
	Inference = "inference"
)

// Config holds the thresholds. Defaults are starting guesses.
type Config struct {
	TrainingMin  int           // node leaves the training pool if any GPU's training score is below this
	RepairMin    int           // a GPU needs repair if its inference score is below this
	DegradeAfter time.Duration // a worse state must hold this long before it's applied
	RecoverAfter time.Duration // a better state must hold this long before it's applied
}

var DefaultConfig = Config{TrainingMin: 80, RepairMin: 50, DegradeAfter: 5 * time.Minute, RecoverAfter: 30 * time.Minute}

// Scores for one node: GPU name -> workload -> score (the agent's <node>.json).
type Scores map[string]map[string]int

// Decision is the applied state of one node after an observation.
type Decision struct {
	Pool       string            // Training or Inference
	PoolReason string            // why, e.g. "gpu-3 training 0 < 80"
	Repair     map[string]string // GPUs that need repair -> reason
	Changes    []string          // what changed in this observation, for events/logs
}

// Tracker remembers per-node state between observations. Not safe for
// concurrent use; the controller calls it from one loop.
type Tracker struct {
	cfg   Config
	nodes map[string]*node
}

func NewTracker(cfg Config) *Tracker { return &Tracker{cfg: cfg, nodes: map[string]*node{}} }

type node struct {
	pool   sticky
	reason string
	repair map[string]*sticky // per GPU: "yes"/"no"
	why    map[string]string
}

// sticky is a value that only changes after the new value has been wanted
// continuously for long enough (longer when getting better than worse).
type sticky struct {
	current, candidate string
	since              time.Time
}

// update returns true if current changed. worse(v) says whether moving to v is
// a degradation (uses DegradeAfter) or a recovery (uses RecoverAfter).
func (s *sticky) update(now time.Time, want string, worse bool, cfg Config) bool {
	if want == s.current {
		s.candidate = ""
		return false
	}
	if want != s.candidate {
		s.candidate, s.since = want, now
	}
	wait := cfg.RecoverAfter
	if worse {
		wait = cfg.DegradeAfter
	}
	if now.Sub(s.since) >= wait {
		s.current, s.candidate = want, ""
		return true
	}
	return false
}

// Observe feeds one round of scores for one node and returns its decision.
func (t *Tracker) Observe(now time.Time, nodeName string, scores Scores) Decision {
	wantPool, wantReason := desiredPool(scores, t.cfg)
	wantRepair := desiredRepair(scores, t.cfg)

	n, seen := t.nodes[nodeName]
	if !seen {
		// First observation (e.g. controller start): apply immediately.
		n = &node{pool: sticky{current: wantPool}, reason: wantReason, repair: map[string]*sticky{}, why: map[string]string{}}
		t.nodes[nodeName] = n
	}

	var changes []string
	if !seen {
		changes = append(changes, fmt.Sprintf("pool %s (%s)", wantPool, wantReason))
	} else if n.pool.update(now, wantPool, wantPool == Inference, t.cfg) {
		changes = append(changes, fmt.Sprintf("pool -> %s (%s)", wantPool, wantReason))
	}
	if n.pool.current == wantPool {
		n.reason = wantReason
	}

	for _, gpu := range sortedGPUs(scores) {
		reason, bad := wantRepair[gpu]
		want := "no"
		if bad {
			want = "yes"
		}
		s, ok := n.repair[gpu]
		if !ok {
			s = &sticky{current: "no"}
			n.repair[gpu] = s
			if !seen && bad {
				s.current = "yes"
				changes = append(changes, fmt.Sprintf("%s needs repair (%s)", gpu, reason))
			}
		}
		if s.update(now, want, bad, t.cfg) {
			if bad {
				changes = append(changes, fmt.Sprintf("%s needs repair (%s)", gpu, reason))
			} else {
				changes = append(changes, fmt.Sprintf("%s repaired", gpu))
			}
		}
		if bad {
			n.why[gpu] = reason
		}
	}

	d := Decision{Pool: n.pool.current, PoolReason: n.reason, Repair: map[string]string{}, Changes: changes}
	for gpu, s := range n.repair {
		if s.current == "yes" {
			d.Repair[gpu] = n.why[gpu]
		}
	}
	return d
}

// desiredPool: inference if any GPU is below the training threshold.
func desiredPool(scores Scores, cfg Config) (string, string) {
	var bad []string
	for _, gpu := range sortedGPUs(scores) {
		if s, ok := scores[gpu]["training"]; ok && s < cfg.TrainingMin {
			bad = append(bad, fmt.Sprintf("%s training %d < %d", gpu, s, cfg.TrainingMin))
		}
	}
	if len(bad) > 0 {
		return Inference, strings.Join(bad, ", ")
	}
	return Training, fmt.Sprintf("all %d GPUs training >= %d", len(scores), cfg.TrainingMin)
}

// desiredRepair: GPUs whose inference score is below RepairMin.
func desiredRepair(scores Scores, cfg Config) map[string]string {
	out := map[string]string{}
	for gpu, byWorkload := range scores {
		if s, ok := byWorkload["inference"]; ok && s < cfg.RepairMin {
			out[gpu] = fmt.Sprintf("inference %d < %d", s, cfg.RepairMin)
		}
	}
	return out
}

func sortedGPUs(scores Scores) []string {
	gpus := make([]string, 0, len(scores))
	for g := range scores {
		gpus = append(gpus, g)
	}
	sort.Strings(gpus)
	return gpus
}
