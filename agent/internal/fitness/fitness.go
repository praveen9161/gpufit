// Package fitness turns per-GPU health measurements into workload-specific
// fitness scores (0-100).
//
// Each component (compute, memory, ...) is a health ratio in [0, 1]:
// measured / expected. 1.0 means "performs like a healthy GPU of this model",
// 0.8 means "20% worse". Producing these ratios from real hardware
// (nvidia-smi, DCGM, bandwidth tests) is the collector's job, not this package's.
//
// A workload score is a weighted geometric mean of the components, times 100.
// Geometric (not arithmetic) so one bad component the workload depends on
// drags the score down hard instead of being averaged away. A workload can
// also set hard floors: if a component is below its floor, the score is 0.
package fitness

import (
	"fmt"
	"math"
	"sort"
)

// Component is one dimension of GPU health.
type Component string

const (
	Compute Component = "compute" // SM throughput vs expected (clocks, power cap)
	Memory  Component = "memory"  // HBM bandwidth / ECC health
	NVLink  Component = "nvlink"  // GPU-to-GPU bandwidth
	Network Component = "network" // node NIC / RDMA bandwidth and latency
	Thermal Component = "thermal" // headroom before thermal throttling
)

// Components lists every component in a stable order.
var Components = []Component{Compute, Memory, NVLink, Network, Thermal}

// Health holds one GPU's component ratios, each in [0, 1].
type Health map[Component]float64

// Profile describes what a workload type cares about.
type Profile struct {
	Name string
	// Weights say how much each component matters. They don't need to sum to 1;
	// they are normalized. A weight of 0 means the workload doesn't care.
	Weights map[Component]float64
	// Floors are hard minimums. Below the floor the GPU is unfit (score 0).
	Floors map[Component]float64
}

// DefaultProfiles are STARTING GUESSES, not measured values. They encode the
// intuition that distributed training is gated by NVLink/network while
// inference mostly needs compute and memory. Tune them from real measurements
// (weeks 5-6).
var DefaultProfiles = []Profile{
	{
		Name:    "training",
		Weights: map[Component]float64{Compute: 0.25, Memory: 0.15, NVLink: 0.25, Network: 0.25, Thermal: 0.10},
		Floors:  map[Component]float64{NVLink: 0.90, Network: 0.90},
	},
	{
		Name:    "finetune",
		Weights: map[Component]float64{Compute: 0.35, Memory: 0.25, NVLink: 0.15, Network: 0.10, Thermal: 0.15},
	},
	{
		Name:    "inference",
		Weights: map[Component]float64{Compute: 0.40, Memory: 0.35, Network: 0.05, Thermal: 0.20},
	},
}

// Result is a workload score plus the reason behind it.
type Result struct {
	Workload string
	Score    int    // 0-100
	Reason   string // human-readable explanation, e.g. "nvlink 0.60 < floor 0.90"
}

// Score computes the fitness of one GPU for one workload profile.
func Score(h Health, p Profile) (Result, error) {
	for _, c := range Components {
		v, ok := h[c]
		if !ok {
			return Result{}, fmt.Errorf("missing component %q", c)
		}
		if v < 0 || v > 1 || math.IsNaN(v) {
			return Result{}, fmt.Errorf("component %q = %v, want value in [0, 1]", c, v)
		}
	}

	// Hard floors first: they override everything else.
	for _, c := range Components {
		if floor, ok := p.Floors[c]; ok && h[c] < floor {
			return Result{
				Workload: p.Name,
				Score:    0,
				Reason:   fmt.Sprintf("%s %.2f < floor %.2f", c, h[c], floor),
			}, nil
		}
	}

	// Weighted geometric mean: exp(sum(w_i * ln(x_i)) / sum(w_i)).
	var sumW, sumLog float64
	for _, c := range Components {
		w := p.Weights[c]
		if w <= 0 {
			continue
		}
		sumW += w
		if h[c] == 0 {
			sumLog = math.Inf(-1) // ln(0); a dead component the workload needs -> 0
		} else {
			sumLog += w * math.Log(h[c])
		}
	}
	if sumW == 0 {
		return Result{}, fmt.Errorf("profile %q has no positive weights", p.Name)
	}
	score := int(math.Round(100 * math.Exp(sumLog/sumW)))

	return Result{Workload: p.Name, Score: score, Reason: weakest(h, p)}, nil
}

// weakest names the relevant component that is costing the most score, or
// "healthy" if every relevant component is at 1.0.
func weakest(h Health, p Profile) string {
	type cost struct {
		c    Component
		loss float64
	}
	var costs []cost
	for _, c := range Components {
		if w := p.Weights[c]; w > 0 && h[c] < 1 {
			costs = append(costs, cost{c, -w * math.Log(h[c])})
		}
	}
	if len(costs) == 0 {
		return "healthy"
	}
	sort.Slice(costs, func(i, j int) bool { return costs[i].loss > costs[j].loss })
	return fmt.Sprintf("weakest: %s %.2f", costs[0].c, h[costs[0].c])
}

// ScoreAll scores every GPU for every default profile:
// GPU name -> workload name -> Result.
func ScoreAll(gpus map[string]Health) (map[string]map[string]Result, error) {
	out := make(map[string]map[string]Result, len(gpus))
	for gpu, h := range gpus {
		out[gpu] = make(map[string]Result, len(DefaultProfiles))
		for _, p := range DefaultProfiles {
			r, err := Score(h, p)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", gpu, err)
			}
			out[gpu][p.Name] = r
		}
	}
	return out, nil
}

// ScoreValues keeps only the numbers: GPU name -> workload name -> score.
// This is the scores.json format the DRA driver reads.
func ScoreValues(results map[string]map[string]Result) map[string]map[string]int {
	out := make(map[string]map[string]int, len(results))
	for gpu, byWorkload := range results {
		out[gpu] = make(map[string]int, len(byWorkload))
		for w, r := range byWorkload {
			out[gpu][w] = r.Score
		}
	}
	return out
}
