// Package report estimates what degraded GPUs cost training jobs: the
// numbers behind `kubectl gpufit report` and the savings metrics.
//
// A synchronous training job runs at its slowest GPU's pace, so every GPU in
// a job that holds a degraded GPU wastes (1 - slowest speed) of its time.
// Speed is estimated from the training score (score / 100). That estimate is
// PESSIMISTIC for mild degradation: in session B a GPU scored 50 but ran at
// 0.84. The report says so; session C compares it with measured waste.
package report

import (
	"sort"
	"time"
)

// Device is one GPU of a job: Node is the DRA pool (= node name), Name the device.
type Device struct {
	Node string `json:"node"`
	Name string `json:"name"`
}

// Job is one training job and the GPUs its pods hold right now.
type Job struct {
	Key     string   `json:"key"` // e.g. "lab/Job/train-2gpu"
	Devices []Device `json:"devices"`
}

// Incident is one job that ran with a degraded GPU.
type Incident struct {
	Job            string    `json:"job"`
	GPUs           int       `json:"gpus"`
	Slowest        string    `json:"slowest"` // "node/gpu"
	SlowestScore   int       `json:"slowestScore"`
	FirstSeen      time.Time `json:"firstSeen"`
	LastSeen       time.Time `json:"lastSeen"`
	GPUHours       float64   `json:"gpuHours"`       // job GPU-hours while degraded
	WastedGPUHours float64   `json:"wastedGpuHours"` // estimated
}

// Summary accumulates since Since. Persisted in the status ConfigMap so a
// controller restart continues the totals.
type Summary struct {
	Since                   time.Time            `json:"since"`
	PricePerGPUHour         float64              `json:"pricePerGpuHour"`
	TrainingGPUHours        float64              `json:"trainingGpuHours"` // all training jobs
	DegradedGPUHours        float64              `json:"degradedGpuHours"` // training jobs holding a degraded GPU
	EstimatedWastedGPUHours float64              `json:"estimatedWastedGpuHours"`
	Incidents               map[string]*Incident `json:"incidents"`
	Rescues                 []Rescue             `json:"rescues,omitempty"` // newest last, at most MaxIncidents
}

// Rescue is one training job GPUFit moved off a degraded GPU.
type Rescue struct {
	Job  string    `json:"job"`
	From string    `json:"from"` // the degraded "node/gpu" that triggered it
	Pods int       `json:"pods"`
	Time time.Time `json:"time"`
}

// AddRescue records a rescue (keeps the newest MaxIncidents).
func (s *Summary) AddRescue(r Rescue) {
	s.Rescues = append(s.Rescues, r)
	if len(s.Rescues) > MaxIncidents {
		s.Rescues = s.Rescues[len(s.Rescues)-MaxIncidents:]
	}
}

// MaxIncidents bounds how many incidents are kept (oldest dropped).
const MaxIncidents = 50

// Scores returns a GPU's training score (0-100) and whether it's known.
type Scores func(node, gpu string) (int, bool)

// Add accounts for dt of wall time in which these jobs held these GPUs.
// A job counts as degraded if any of its GPUs scores below threshold.
func (s *Summary) Add(now time.Time, dt time.Duration, jobs []Job, score Scores, threshold int) {
	if s.Incidents == nil {
		s.Incidents = map[string]*Incident{}
	}
	h := dt.Hours()
	for _, j := range jobs {
		g := float64(len(j.Devices))
		if g == 0 {
			continue
		}
		s.TrainingGPUHours += g * h
		slowest, min, known := "", 101, false
		for _, d := range j.Devices {
			if sc, ok := score(d.Node, d.Name); ok && sc < min {
				slowest, min, known = d.Node+"/"+d.Name, sc, true
			}
		}
		if !known || min >= threshold {
			continue
		}
		waste := g * (1 - float64(min)/100) * h
		s.DegradedGPUHours += g * h
		s.EstimatedWastedGPUHours += waste
		inc, ok := s.Incidents[j.Key]
		if !ok {
			inc = &Incident{Job: j.Key, FirstSeen: now}
			s.Incidents[j.Key] = inc
		}
		inc.GPUs, inc.Slowest, inc.SlowestScore, inc.LastSeen = len(j.Devices), slowest, min, now
		inc.GPUHours += g * h
		inc.WastedGPUHours += waste
	}
	s.trim()
}

// EstimatedWastedDollars at the configured price.
func (s *Summary) EstimatedWastedDollars() float64 {
	return s.EstimatedWastedGPUHours * s.PricePerGPUHour
}

// SortedIncidents returns incidents, most wasteful first.
func (s *Summary) SortedIncidents() []*Incident {
	out := make([]*Incident, 0, len(s.Incidents))
	for _, i := range s.Incidents {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].WastedGPUHours > out[b].WastedGPUHours })
	return out
}

func (s *Summary) trim() {
	if len(s.Incidents) <= MaxIncidents {
		return
	}
	all := make([]*Incident, 0, len(s.Incidents))
	for _, i := range s.Incidents {
		all = append(all, i)
	}
	sort.Slice(all, func(a, b int) bool { return all[a].LastSeen.Before(all[b].LastSeen) })
	for _, i := range all[:len(all)-MaxIncidents] {
		delete(s.Incidents, i.Job)
	}
}
