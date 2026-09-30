package pools

import (
	"strings"
	"testing"
	"time"
)

var cfg = Config{TrainingMin: 80, RepairMin: 50, DegradeAfter: 5 * time.Minute, RecoverAfter: 30 * time.Minute}

func healthy() Scores {
	s := Scores{}
	for _, g := range []string{"gpu-0", "gpu-1", "gpu-2"} {
		s[g] = map[string]int{"training": 100, "finetune": 100, "inference": 100}
	}
	return s
}

func degraded() Scores { // gpu-1 is a straggler: bad for training, fine for inference
	s := healthy()
	s["gpu-1"] = map[string]int{"training": 70, "finetune": 88, "inference": 85}
	return s
}

func broken() Scores { // gpu-2 has ECC errors: unfit for everything
	s := healthy()
	s["gpu-2"] = map[string]int{"training": 0, "finetune": 0, "inference": 0}
	return s
}

func TestFirstObservationAppliesImmediately(t *testing.T) {
	tr := NewTracker(cfg)
	d := tr.Observe(time.Now(), "node-a", degraded())
	if d.Pool != Inference || !strings.Contains(d.PoolReason, "gpu-1 training 70 < 80") {
		t.Errorf("got %s (%s), want inference because of gpu-1", d.Pool, d.PoolReason)
	}
	if len(d.Changes) != 1 {
		t.Errorf("changes = %v, want the initial pool", d.Changes)
	}
}

func TestDegradeWaitsDegradeAfter(t *testing.T) {
	tr := NewTracker(cfg)
	t0 := time.Now()
	tr.Observe(t0, "n", healthy())

	for _, tc := range []struct {
		at   time.Duration
		want string
	}{
		{1 * time.Minute, Training},  // degraded for 0s so far
		{4 * time.Minute, Training},  // 3m < 5m
		{6 * time.Minute, Inference}, // 5m reached
	} {
		if d := tr.Observe(t0.Add(tc.at), "n", degraded()); d.Pool != tc.want {
			t.Errorf("t=%s: pool %s, want %s", tc.at, d.Pool, tc.want)
		}
	}
}

func TestRecoveryWaitsLongerAndResetsOnRelapse(t *testing.T) {
	tr := NewTracker(cfg)
	t0 := time.Now()
	tr.Observe(t0, "n", degraded()) // starts in inference

	if d := tr.Observe(t0.Add(1*time.Minute), "n", healthy()); d.Pool != Inference {
		t.Fatalf("recovered after 0s")
	}
	if d := tr.Observe(t0.Add(20*time.Minute), "n", healthy()); d.Pool != Inference {
		t.Fatalf("recovered after 19m, want to wait 30m")
	}
	// Relapse for one observation: the recovery clock starts over.
	tr.Observe(t0.Add(21*time.Minute), "n", degraded())
	if d := tr.Observe(t0.Add(40*time.Minute), "n", healthy()); d.Pool != Inference {
		t.Fatalf("recovered 19m after relapse, want 30m")
	}
	d := tr.Observe(t0.Add(71*time.Minute), "n", healthy())
	if d.Pool != Training || len(d.Changes) != 1 {
		t.Fatalf("t=71m: pool %s changes %v, want training after 31m healthy", d.Pool, d.Changes)
	}
}

func TestFlappingNeverMovesTheNode(t *testing.T) {
	tr := NewTracker(cfg)
	t0 := time.Now()
	tr.Observe(t0, "n", healthy())
	for i := 1; i <= 60; i++ { // alternates every minute for an hour
		s := healthy()
		if i%2 == 0 {
			s = degraded()
		}
		if d := tr.Observe(t0.Add(time.Duration(i)*time.Minute), "n", s); d.Pool != Training {
			t.Fatalf("minute %d: node moved to %s while flapping", i, d.Pool)
		}
	}
}

func TestRepairIsPerGPU(t *testing.T) {
	tr := NewTracker(cfg)
	t0 := time.Now()
	tr.Observe(t0, "n", healthy())
	tr.Observe(t0.Add(time.Minute), "n", broken())
	d := tr.Observe(t0.Add(7*time.Minute), "n", broken())
	if len(d.Repair) != 1 || d.Repair["gpu-2"] == "" {
		t.Errorf("repair = %v, want only gpu-2", d.Repair)
	}
	if d.Pool != Inference {
		t.Errorf("pool = %s, want inference (gpu-2 training 0)", d.Pool)
	}
	if len(d.Changes) != 2 {
		t.Errorf("changes = %v, want pool change + gpu-2 needs repair", d.Changes)
	}
}

func TestNodesAreIndependent(t *testing.T) {
	tr := NewTracker(cfg)
	now := time.Now()
	if d := tr.Observe(now, "a", degraded()); d.Pool != Inference {
		t.Errorf("a: %s", d.Pool)
	}
	if d := tr.Observe(now, "b", healthy()); d.Pool != Training {
		t.Errorf("b: %s", d.Pool)
	}
}
