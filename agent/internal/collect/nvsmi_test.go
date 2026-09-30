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

// Real captures: AWS g4dn.xlarge, Tesla T4, driver 595.91.07 (session A,
// 2026-09-30). On this driver applications clocks are "deprecated", so the
// clock ratio must come out n/a instead of breaking the parse.
func TestRealT4Captures(t *testing.T) {
	idle := readings(t, "real-t4-idle.xml")
	if len(idle) != 1 || idle[0].Product != "Tesla T4" {
		t.Fatalf("idle: %+v", idle)
	}
	r := idle[0]
	if r.PowerLimitW != 70 || r.DefaultPowerLimitW != 70 || r.SlowdownTempC <= 0 {
		t.Errorf("idle numbers: power %v/%v slowdown %v", r.PowerLimitW, r.DefaultPowerLimitW, r.SlowdownTempC)
	}
	if !math.IsNaN(r.GraphicsMHz) {
		t.Errorf("applications clock = %v, want NaN (deprecated on this driver)", r.GraphicsMHz)
	}
	e := Ratios(r, DefaultParams)
	if e.Health[fitness.Compute] != 1 || !strings.Contains(e.Why[fitness.Compute], "graphics clock n/a") {
		t.Errorf("idle compute = %v (%s)", e.Health[fitness.Compute], e.Why[fitness.Compute])
	}

	capped := readings(t, "real-t4-power60w-gemm.xml")[0]
	ec := Ratios(capped, DefaultParams)
	// 60/70 W. Measured on this GPU: GEMM 0.83, train 0.83 of baseline.
	if !near(ec.Health[fitness.Compute], 60.0/70) {
		t.Errorf("60 W compute = %.3f (%s), want %.3f", ec.Health[fitness.Compute], ec.Why[fitness.Compute], 60.0/70)
	}
	if got := capped.ThrottleCountersUS["sw_power_cap"]; got != 48339072 {
		t.Errorf("sw_power_cap counter = %v us, want 48339072", got)
	}
	if capped.PowerDrawW <= 0 {
		t.Errorf("instant power draw = %v, want > 0", capped.PowerDrawW)
	}
	if len(capped.ActiveEvents) == 0 || capped.ActiveEvents[0] != "sw_power_cap" {
		t.Errorf("events under load = %v, want sw_power_cap", capped.ActiveEvents)
	}
}

// The "unexplained clock deficit" rule on real T4 captures (session A). NOTE:
// the rule was designed on this same data; it still needs validating on new data.
func TestClockDeficitRealT4(t *testing.T) {
	for _, tc := range []struct {
		file     string
		measured float64 // throughput vs baseline, measured in session A
		flagged  bool
	}{
		{"real-t4-clock50pct-train.xml", 0.86, true},  // locked at 795 MHz, 60.6/70 W, no reason
		{"real-t4-clock70pct-train.xml", 0.99, false}, // lock not binding: power-capped
		{"real-t4-power60w-gemm.xml", 0.83, false},    // low clock explained by sw_power_cap
		{"real-t4-idle.xml", 1.00, false},             // not busy
	} {
		e := Ratios(readings(t, tc.file)[0], DefaultParams)
		flagged := strings.Contains(e.Why[fitness.Compute], "UNEXPLAINED")
		if flagged != tc.flagged {
			t.Errorf("%s: flagged=%t, want %t (%s)", tc.file, flagged, tc.flagged, e.Why[fitness.Compute])
		}
		if tc.flagged && math.Abs(e.Health[fitness.Compute]-tc.measured) > 0.05 {
			t.Errorf("%s: compute %.2f, measured %.2f: estimate off by more than 0.05", tc.file, e.Health[fitness.Compute], tc.measured)
		}
	}
}
