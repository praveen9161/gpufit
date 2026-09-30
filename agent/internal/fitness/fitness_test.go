package fitness

import (
	"strings"
	"testing"
)

func healthy() Health {
	return Health{Compute: 1, Memory: 1, NVLink: 1, Network: 1, Thermal: 1}
}

func profile(t *testing.T, name string) Profile {
	t.Helper()
	for _, p := range DefaultProfiles {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no profile %q", name)
	return Profile{}
}

func TestHealthyGPUScores100Everywhere(t *testing.T) {
	for _, p := range DefaultProfiles {
		r, err := Score(healthy(), p)
		if err != nil {
			t.Fatal(err)
		}
		if r.Score != 100 || r.Reason != "healthy" {
			t.Errorf("%s: got %d (%s), want 100 (healthy)", p.Name, r.Score, r.Reason)
		}
	}
}

// The core GPUFit idea: a GPU with broken NVLink is unfit for distributed
// training but still fine for inference.
func TestBadNVLinkKillsTrainingButNotInference(t *testing.T) {
	h := healthy()
	h[NVLink] = 0.6

	tr, _ := Score(h, profile(t, "training"))
	if tr.Score != 0 || !strings.Contains(tr.Reason, "nvlink") {
		t.Errorf("training: got %d (%s), want 0 due to nvlink floor", tr.Score, tr.Reason)
	}

	inf, _ := Score(h, profile(t, "inference"))
	if inf.Score != 100 {
		t.Errorf("inference: got %d, want 100 (inference ignores nvlink)", inf.Score)
	}

	ft, _ := Score(h, profile(t, "finetune"))
	if ft.Score <= 0 || ft.Score >= 100 {
		t.Errorf("finetune: got %d, want degraded but usable (0 < score < 100)", ft.Score)
	}
}

func TestLowerHealthNeverRaisesScore(t *testing.T) {
	for _, p := range DefaultProfiles {
		for _, c := range Components {
			prev := 101
			for _, v := range []float64{1, 0.95, 0.9, 0.7, 0.5, 0.2, 0} {
				h := healthy()
				h[c] = v
				r, err := Score(h, p)
				if err != nil {
					t.Fatal(err)
				}
				if r.Score > prev {
					t.Errorf("%s/%s=%.2f: score rose to %d from %d", p.Name, c, v, r.Score, prev)
				}
				prev = r.Score
			}
		}
	}
}

func TestRejectsBadInput(t *testing.T) {
	h := healthy()
	delete(h, Thermal)
	if _, err := Score(h, DefaultProfiles[0]); err == nil {
		t.Error("want error for missing component")
	}
	h = healthy()
	h[Memory] = 1.5
	if _, err := Score(h, DefaultProfiles[0]); err == nil {
		t.Error("want error for out-of-range component")
	}
}

// Synchronous training runs at the speed of its slowest GPU, so a GPU that is
// throttling must be scored WORST for training, not averaged away.
func TestStragglerHurtsTrainingMost(t *testing.T) {
	h := healthy()
	h[Compute] = 0.5 // e.g. hardware slowdown

	tr, _ := Score(h, profile(t, "training"))
	if tr.Score != 50 || !strings.Contains(tr.Reason, "weakest link: compute") {
		t.Errorf("training: got %d (%s), want 50 (weakest link: compute)", tr.Score, tr.Reason)
	}
	for _, name := range []string{"finetune", "inference"} {
		r, _ := Score(h, profile(t, name))
		if r.Score <= tr.Score {
			t.Errorf("%s: got %d, want more than training's %d (only this GPU's share is lost)", name, r.Score, tr.Score)
		}
	}
}
