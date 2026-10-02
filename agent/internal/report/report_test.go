package report

import (
	"math"
	"strings"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Session B's situation: 2-GPU job, one GPU degraded (training score 50).
func TestStragglerWaste(t *testing.T) {
	scores := map[string]int{"a/gpu-0": 100, "b/gpu-0": 50}
	score := func(n, g string) (int, bool) { v, ok := scores[n+"/"+g]; return v, ok }
	job := Job{Key: "lab/Job/train", Devices: []Device{{"a", "gpu-0"}, {"b", "gpu-0"}}}
	healthy := Job{Key: "lab/Job/other", Devices: []Device{{"a", "gpu-1"}}}
	scores["a/gpu-1"] = 100

	s := Summary{PricePerGPUHour: 2}
	t0 := time.Now()
	s.Add(t0, 30*time.Minute, []Job{job, healthy}, score, 80)
	s.Add(t0.Add(30*time.Minute), 30*time.Minute, []Job{job, healthy}, score, 80)

	// 1 h: training total = 2 + 1 GPU-h; degraded job = 2 GPU-h; waste = 2 x (1 - 0.5) x 1 h = 1 GPU-h.
	if !near(s.TrainingGPUHours, 3) || !near(s.DegradedGPUHours, 2) || !near(s.EstimatedWastedGPUHours, 1) {
		t.Errorf("totals: training %v degraded %v wasted %v", s.TrainingGPUHours, s.DegradedGPUHours, s.EstimatedWastedGPUHours)
	}
	if !near(s.EstimatedWastedDollars(), 2) {
		t.Errorf("dollars = %v, want 2", s.EstimatedWastedDollars())
	}
	inc := s.Incidents["lab/Job/train"]
	if inc == nil || inc.Slowest != "b/gpu-0" || inc.SlowestScore != 50 || inc.GPUs != 2 || !inc.FirstSeen.Equal(t0) {
		t.Errorf("incident = %+v", inc)
	}
	if _, ok := s.Incidents["lab/Job/other"]; ok {
		t.Error("healthy job reported as incident")
	}
}

func TestUnknownScoresAndThreshold(t *testing.T) {
	score := func(n, g string) (int, bool) {
		if g == "gpu-9" {
			return 0, false
		}
		return 85, true
	}
	s := Summary{}
	s.Add(time.Now(), time.Hour, []Job{{Key: "j", Devices: []Device{{"a", "gpu-0"}, {"a", "gpu-9"}}}}, score, 80)
	if s.DegradedGPUHours != 0 || len(s.Incidents) != 0 {
		t.Errorf("85 >= 80 and an unknown GPU must not count: %+v", s)
	}
}

func TestIncidentsAreBounded(t *testing.T) {
	score := func(n, g string) (int, bool) { return 10, true }
	s := Summary{}
	now := time.Now()
	for i := 0; i < MaxIncidents+7; i++ {
		s.Add(now.Add(time.Duration(i)*time.Second), time.Second, []Job{{Key: string(rune('A'+i%26)) + time.Duration(i).String(), Devices: []Device{{"a", "gpu-0"}}}}, score, 80)
	}
	if len(s.Incidents) != MaxIncidents {
		t.Errorf("kept %d incidents, want %d", len(s.Incidents), MaxIncidents)
	}
}

func TestRender(t *testing.T) {
	t0 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	score := func(n, g string) (int, bool) { return map[string]int{"b/gpu-0": 50}[n+"/"+g], n+"/"+g == "b/gpu-0" }
	s := Summary{Since: t0, PricePerGPUHour: 2.5}
	s.Add(t0.Add(time.Hour), time.Hour, []Job{{Key: "lab/Job/train", Devices: []Device{{"b", "gpu-0"}, {"a", "gpu-0"}}}}, score, 80)
	var b strings.Builder
	s.Render(&b, t0.Add(time.Hour), "observe")
	for _, want := range []string{"since 2026-10-02 12:00 UTC (1h0m0s)", "2.00   (100.0%)", "1.00 GPU-hours", "$2.50 at $2.50/GPU-hour", "lab/Job/train", "b/gpu-0", "Observe mode"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}
}
