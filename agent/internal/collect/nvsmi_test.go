package collect

// Test data in testdata/nvidia-smi/ is HAND-WRITTEN from the documented
// layout, not captured from real GPUs. Real captures come from the GPU session.

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/praveen9161/gpufit/agent/internal/fitness"
)

func readings(t *testing.T, file string) []Reading {
	t.Helper()
	data, err := os.ReadFile("../../testdata/nvidia-smi/" + file)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseXML(data)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestParseNewerNames(t *testing.T) {
	r := readings(t, "healthy-2gpu.xml")
	if len(r) != 2 || r[0].Name != "gpu-0" || r[1].Name != "gpu-1" {
		t.Fatalf("got %d GPUs %+v", len(r), r)
	}
	g := r[1]
	if g.PowerLimitW != 300 || g.DefaultPowerLimitW != 300 || g.TempC != 78 || g.SlowdownTempC != 95 {
		t.Errorf("gpu-1 numbers: %+v", g)
	}
	if g.UncorrectableECC != 0 {
		t.Errorf("gpu-1 ECC = %v, want 0", g.UncorrectableECC)
	}
	if len(g.ActiveEvents) != 1 || g.ActiveEvents[0] != "sw_power_cap" {
		t.Errorf("gpu-1 events = %v, want [sw_power_cap]", g.ActiveEvents)
	}
	if g.UUID != "GPU-11111111-1111-1111-1111-111111111111" {
		t.Errorf("gpu-1 uuid = %q", g.UUID)
	}
}

func TestParseOlderNames(t *testing.T) {
	g := readings(t, "power-capped-old-names.xml")[0]
	if g.PowerLimitW != 210 || g.DefaultPowerLimitW != 300 {
		t.Errorf("power = %v/%v, want 210/300 from power_readings", g.PowerLimitW, g.DefaultPowerLimitW)
	}
	if !math.IsNaN(g.UncorrectableECC) {
		t.Errorf("ECC = %v, want NaN for N/A", g.UncorrectableECC)
	}
	if !math.IsNaN(g.GraphicsMHz) {
		t.Errorf("clock = %v, want NaN for N/A", g.GraphicsMHz)
	}
	if len(g.ActiveEvents) != 1 || g.ActiveEvents[0] != "sw_power_cap" {
		t.Errorf("events = %v, want [sw_power_cap] from clocks_throttle_reasons", g.ActiveEvents)
	}
}

func TestRatiosHealthy(t *testing.T) {
	for _, r := range readings(t, "healthy-2gpu.xml") {
		e := Ratios(r, DefaultParams)
		// gpu-1 runs at its power cap under load: sw_power_cap must NOT hurt compute.
		for _, c := range fitness.Components {
			if e.Health[c] != 1 {
				t.Errorf("%s %s = %.2f (%s), want 1", r.Name, c, e.Health[c], e.Why[c])
			}
		}
	}
}

func TestRatiosPowerCap(t *testing.T) {
	e := Ratios(readings(t, "power-capped-old-names.xml")[0], DefaultParams)
	if !near(e.Health[fitness.Compute], 0.70) {
		t.Errorf("compute = %.2f (%s), want 0.70 for 210/300 W", e.Health[fitness.Compute], e.Why[fitness.Compute])
	}
	if !strings.Contains(e.Why[fitness.Memory], "ECC n/a") {
		t.Errorf("memory why = %q, want ECC n/a", e.Why[fitness.Memory])
	}
	// The exponent bends the curve; it's what the GPU session measures.
	p := DefaultParams
	p.PowerExponent = 0.5
	if e := Ratios(readings(t, "power-capped-old-names.xml")[0], p); !near(e.Health[fitness.Compute], math.Sqrt(0.7)) {
		t.Errorf("compute with exponent 0.5 = %.3f, want %.3f", e.Health[fitness.Compute], math.Sqrt(0.7))
	}
}

func TestRatiosDegraded(t *testing.T) {
	r := readings(t, "degraded-2gpu.xml")

	hot := Ratios(r[0], DefaultParams)
	// 2 C headroom / 15 C band = 0.13, below the 0.5 event cap.
	if !near(hot.Health[fitness.Thermal], 2.0/15) {
		t.Errorf("hot thermal = %.2f (%s), want 0.13", hot.Health[fitness.Thermal], hot.Why[fitness.Thermal])
	}
	if !near(hot.Health[fitness.Compute], 0.5) {
		t.Errorf("hot compute = %.2f (%s), want 0.5 (hw_slowdown)", hot.Health[fitness.Compute], hot.Why[fitness.Compute])
	}

	ecc := Ratios(r[1], DefaultParams)
	if ecc.Health[fitness.Memory] != 0 || !strings.Contains(ecc.Why[fitness.Memory], "3 uncorrectable") {
		t.Errorf("ecc memory = %.2f (%s), want 0 with 3 uncorrectable", ecc.Health[fitness.Memory], ecc.Why[fitness.Memory])
	}
}

func TestParseXMLRejectsBadInput(t *testing.T) {
	for _, in := range []string{"not xml", "<other/>", "<nvidia_smi_log></nvidia_smi_log>"} {
		if _, err := ParseXML([]byte(in)); err == nil {
			t.Errorf("ParseXML(%q) = nil error", in)
		}
	}
}

func TestNvidiaSMICollector(t *testing.T) {
	data, _ := os.ReadFile("../../testdata/nvidia-smi/degraded-2gpu.xml")
	var gotArgs []string
	c := NvidiaSMI{Params: DefaultParams, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string{name}, args...)
		return data, nil
	}}
	gpus, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(gotArgs, " ") != "nvidia-smi -q -x" {
		t.Errorf("ran %q", gotArgs)
	}
	if len(gpus) != 2 || gpus["gpu-1"][fitness.Memory] != 0 {
		t.Errorf("got %v", gpus)
	}
}

// fixed returns canned results in order.
type fixed struct{ results []map[string]fitness.Health }

func (f *fixed) Collect(context.Context) (map[string]fitness.Health, error) {
	r := f.results[0]
	f.results = f.results[1:]
	return r, nil
}

func healthy() fitness.Health {
	return fitness.Health{fitness.Compute: 1, fitness.Memory: 1, fitness.NVLink: 1, fitness.Network: 1, fitness.Thermal: 1}
}

func TestWindowedKeepsWorstUntilItExpires(t *testing.T) {
	hot := healthy()
	hot[fitness.Thermal] = 0.2
	inner := &fixed{results: []map[string]fitness.Health{
		{"gpu-0": healthy()}, // t=0
		{"gpu-0": hot},       // t=1m: under load, hot
		{"gpu-0": healthy()}, // t=2m: idle again
		{"gpu-0": healthy()}, // t=12m: the hot sample is > 10m old
	}}
	start := time.Now()
	now := start
	w := &Windowed{Inner: inner, Window: 10 * time.Minute, Now: func() time.Time { return now }}

	for i, tc := range []struct {
		at   time.Duration
		want float64
	}{{0, 1}, {time.Minute, 0.2}, {2 * time.Minute, 0.2}, {12 * time.Minute, 1}} {
		now = start.Add(tc.at)
		got, err := w.Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got["gpu-0"][fitness.Thermal] != tc.want {
			t.Errorf("step %d (t=%s): thermal = %v, want %v", i, tc.at, got["gpu-0"][fitness.Thermal], tc.want)
		}
	}
}
