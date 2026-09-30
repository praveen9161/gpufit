package collect

// nvidia-smi collector. Design and the reasoning behind every rule:
// docs/design/nvidia-smi-collector.md. All thresholds are starting
// assumptions until measured on real hardware.

import (
	"context"
	"encoding/xml"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/praveen9161/gpufit/agent/internal/fitness"
)

// Reading is what one `nvidia-smi -q -x` report says about one GPU.
// Unknown numbers ("N/A", missing element) are NaN.
type Reading struct {
	Name    string // DRA device name: "gpu-<index>", index = order in the report
	UUID    string
	Product string

	PowerLimitW, DefaultPowerLimitW float64 // enforced vs default power limit
	GraphicsMHz, DefaultGraphicsMHz float64 // applications clocks vs defaults
	MemMHz, DefaultMemMHz           float64
	TempC, SlowdownTempC            float64

	// Clock event ("throttle") reasons that are active, e.g. "hw_slowdown",
	// "sw_power_cap", "hw_thermal_slowdown".
	ActiveEvents []string

	// Cumulative microseconds per throttle reason (clocks_event_reasons_counters),
	// e.g. "sw_power_cap". Recorded and shown, NOT used for scoring yet: on the
	// T4 (driver 595.91.07) sw_power_cap stayed 0 while the flag was Active at
	// the default limit, and only counted once the limit was lowered. Semantics
	// need more measurements first. nil if the driver doesn't report them.
	ThrottleCountersUS map[string]float64

	PowerDrawW float64 // instant power draw (informational)

	UncorrectableECC  float64 // volatile (since driver load); NaN if ECC is off/unsupported
	PendingRetirement bool    // retired pages waiting for a GPU reset
	RemapPending      bool    // row remapping waiting for a GPU reset
	RemapFailure      bool
}

// Params are the tunable parts of the mapping. Defaults are guesses.
type Params struct {
	PowerExponent     float64 // compute ~ power_ratio^PowerExponent; to be measured
	ThermalBandC      float64 // thermal = (slowdown - temp) / band
	HWSlowdownPenalty float64 // compute multiplier while a HW slowdown is active
	ThermalEventCap   float64 // thermal is at most this while a thermal slowdown is active
	PendingMemory     float64 // memory ratio while retirement/remap waits for a reset
}

var DefaultParams = Params{
	PowerExponent:     1.0,
	ThermalBandC:      15,
	HWSlowdownPenalty: 0.5,
	ThermalEventCap:   0.5,
	PendingMemory:     0.5,
}

// Explained is a Health plus one human-readable reason per component.
type Explained struct {
	Health fitness.Health
	Why    map[fitness.Component]string
}

// Ratios maps one Reading to the five health ratios.
func Ratios(r Reading, p Params) Explained {
	e := Explained{Health: fitness.Health{}, Why: map[fitness.Component]string{}}
	set := func(c fitness.Component, v float64, why string, args ...any) {
		e.Health[c] = clamp01(v)
		e.Why[c] = fmt.Sprintf(why, args...)
	}

	// compute: power cap and clock cap, halved during a hardware slowdown.
	// sw_power_cap is NOT penalized: running at the power limit under load is
	// normal; a lowered limit already shows up in the power ratio.
	power, powerWhy := ratio(r.PowerLimitW, r.DefaultPowerLimitW, "power limit", "W")
	clocks, clocksWhy := ratio(r.GraphicsMHz, r.DefaultGraphicsMHz, "graphics clock", "MHz")
	compute := math.Pow(power, p.PowerExponent) * clocks
	why := powerWhy + "; " + clocksWhy
	if ev := active(r, "hw_slowdown", "hw_power_brake_slowdown"); ev != "" {
		compute *= p.HWSlowdownPenalty
		why += fmt.Sprintf("; %s active -> x%.1f", ev, p.HWSlowdownPenalty)
	}
	set(fitness.Compute, compute, "%s", why)

	// memory: errors first, then the memory clock cap.
	switch {
	case r.RemapFailure:
		set(fitness.Memory, 0, "row remapping FAILED")
	case r.UncorrectableECC > 0:
		set(fitness.Memory, 0, "%.0f uncorrectable ECC errors since driver load", r.UncorrectableECC)
	case r.PendingRetirement || r.RemapPending:
		set(fitness.Memory, p.PendingMemory, "page retirement / row remap pending, needs GPU reset")
	default:
		mem, memWhy := ratio(r.MemMHz, r.DefaultMemMHz, "memory clock", "MHz")
		ecc := "no uncorrectable ECC errors"
		if math.IsNaN(r.UncorrectableECC) {
			ecc = "ECC n/a"
		}
		set(fitness.Memory, mem, "%s; %s", ecc, memWhy)
	}

	set(fitness.NVLink, 1, "not collected in v1 (needs `nvidia-smi nvlink`; verify format on real GPU)")
	set(fitness.Network, 1, "not measured (nvidia-smi can't see NICs)")

	// thermal: headroom below the slowdown temperature.
	if math.IsNaN(r.TempC) || math.IsNaN(r.SlowdownTempC) {
		set(fitness.Thermal, 1, "temperature or slowdown threshold n/a")
	} else {
		th := (r.SlowdownTempC - r.TempC) / p.ThermalBandC
		why := fmt.Sprintf("%.0f C, slowdown at %.0f C (%.0f C headroom, full score at >= %.0f C)",
			r.TempC, r.SlowdownTempC, r.SlowdownTempC-r.TempC, p.ThermalBandC)
		if ev := active(r, "hw_thermal_slowdown", "sw_thermal_slowdown"); ev != "" {
			th = math.Min(th, p.ThermalEventCap)
			why += fmt.Sprintf("; %s active -> at most %.1f", ev, p.ThermalEventCap)
		}
		set(fitness.Thermal, th, "%s", why)
	}
	return e
}

// ratio returns have/want (1 if either is unknown) and an explanation.
func ratio(have, want float64, what, unit string) (float64, string) {
	if math.IsNaN(have) || math.IsNaN(want) || want <= 0 {
		return 1, what + " n/a"
	}
	return math.Min(have/want, 1), fmt.Sprintf("%s %.0f/%.0f %s (%.0f%%)", what, have, want, unit, 100*have/want)
}

// active returns the first of the given event reasons that is active, or "".
func active(r Reading, reasons ...string) string {
	for _, want := range reasons {
		for _, got := range r.ActiveEvents {
			if got == want {
				return got
			}
		}
	}
	return ""
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// ---- XML parsing ---------------------------------------------------------------
//
// Parsed into a generic tree, not fixed structs, because element names differ
// across driver versions (e.g. clocks_throttle_reasons -> clocks_event_reasons,
// power_readings -> gpu_power_readings). Lookups try each known name.

type node struct {
	XMLName xml.Name
	Text    string `xml:",chardata"`
	Nodes   []node `xml:",any"`
}

func (n *node) child(name string) *node {
	for i := range n.Nodes {
		if n.Nodes[i].XMLName.Local == name {
			return &n.Nodes[i]
		}
	}
	return nil
}

// text returns the text at the first path that exists, e.g. "temperature/gpu_temp".
func (n *node) text(paths ...string) (string, bool) {
	for _, p := range paths {
		cur := n
		for _, part := range strings.Split(p, "/") {
			if cur = cur.child(part); cur == nil {
				break
			}
		}
		if cur != nil {
			return strings.TrimSpace(cur.Text), true
		}
	}
	return "", false
}

var leadingNumber = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?`)

// num parses "300.00 W", "95 C", "1710 MHz" at the first existing path ->
// number; "N/A" or missing -> NaN.
func (n *node) num(paths ...string) float64 {
	s, ok := n.text(paths...)
	if !ok {
		return math.NaN()
	}
	return parseNum(s)
}

func parseNum(s string) float64 {
	m := leadingNumber.FindString(strings.TrimSpace(s))
	if m == "" {
		return math.NaN()
	}
	v, err := strconv.ParseFloat(m, 64)
	if err != nil {
		return math.NaN()
	}
	return v
}

func (n *node) yes(paths ...string) bool {
	s, _ := n.text(paths...)
	return strings.EqualFold(s, "Yes")
}

// ParseXML reads `nvidia-smi -q -x` output.
func ParseXML(data []byte) ([]Reading, error) {
	var root node
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse nvidia-smi XML: %w", err)
	}
	if root.XMLName.Local != "nvidia_smi_log" {
		return nil, fmt.Errorf("parse nvidia-smi XML: root is <%s>, want <nvidia_smi_log>", root.XMLName.Local)
	}

	var out []Reading
	for i := range root.Nodes {
		g := &root.Nodes[i]
		if g.XMLName.Local != "gpu" {
			continue
		}
		r := Reading{
			Name:               fmt.Sprintf("gpu-%d", len(out)),
			PowerLimitW:        g.num("gpu_power_readings/current_power_limit", "power_readings/enforced_power_limit", "power_readings/power_limit"),
			DefaultPowerLimitW: g.num("gpu_power_readings/default_power_limit", "power_readings/default_power_limit"),
			GraphicsMHz:        g.num("applications_clocks/graphics_clock"),
			DefaultGraphicsMHz: g.num("default_applications_clocks/graphics_clock"),
			MemMHz:             g.num("applications_clocks/mem_clock"),
			DefaultMemMHz:      g.num("default_applications_clocks/mem_clock"),
			TempC:              g.num("temperature/gpu_temp"),
			SlowdownTempC:      g.num("temperature/gpu_temp_slow_threshold"),
			UncorrectableECC:   uncorrectableECC(g),
			PendingRetirement:  g.yes("retired_pages/pending_blacklist", "retired_pages/pending_retirement"),
			RemapPending:       g.yes("remapped_rows/remapped_row_pending"),
			RemapFailure:       g.yes("remapped_rows/remapped_row_failure"),
		}
		r.UUID, _ = g.text("uuid")
		r.Product, _ = g.text("product_name")
		r.ActiveEvents = activeEvents(g)
		r.ThrottleCountersUS = throttleCounters(g)
		r.PowerDrawW = g.num("gpu_power_readings/instant_power_draw", "power_readings/power_draw")
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("parse nvidia-smi XML: no <gpu> elements")
	}
	return out, nil
}

func activeEvents(g *node) []string {
	var out []string
	for _, block := range []string{"clocks_event_reasons", "clocks_throttle_reasons"} {
		b := g.child(block)
		if b == nil {
			continue
		}
		for _, c := range b.Nodes {
			if strings.TrimSpace(c.Text) != "Active" {
				continue
			}
			name := strings.TrimPrefix(c.XMLName.Local, "clocks_event_reason_")
			name = strings.TrimPrefix(name, "clocks_throttle_reason_")
			out = append(out, name)
		}
	}
	return out
}

func throttleCounters(g *node) map[string]float64 {
	b := g.child("clocks_event_reasons_counters")
	if b == nil {
		return nil
	}
	out := map[string]float64{}
	for _, c := range b.Nodes {
		if v := parseNum(c.Text); !math.IsNaN(v) {
			out[strings.TrimPrefix(c.XMLName.Local, "clocks_event_reasons_counters_")] = v
		}
	}
	return out
}

// uncorrectableECC sums volatile uncorrectable counts (newer drivers:
// dram_uncorrectable/sram_uncorrectable; older: double_bit/total).
// NaN if nothing is reported (ECC disabled or unsupported).
func uncorrectableECC(g *node) float64 {
	vol := g.child("ecc_errors")
	if vol != nil {
		vol = vol.child("volatile")
	}
	if vol == nil {
		return math.NaN()
	}
	sum, found := 0.0, false
	for i := range vol.Nodes {
		c := &vol.Nodes[i]
		if strings.HasSuffix(c.XMLName.Local, "_uncorrectable") {
			if v := parseNum(c.Text); !math.IsNaN(v) {
				sum, found = sum+v, true
			}
		}
	}
	if v := vol.num("double_bit/total"); !math.IsNaN(v) {
		sum, found = sum+v, true
	}
	if !found {
		return math.NaN()
	}
	return sum
}

// ---- collector ---------------------------------------------------------------

// NvidiaSMI runs `nvidia-smi -q -x` and maps each GPU to health ratios.
type NvidiaSMI struct {
	Command string // default "nvidia-smi"
	Params  Params
	// Run executes the command; tests replace it. Default: os/exec.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (c NvidiaSMI) Collect(ctx context.Context) (map[string]fitness.Health, error) {
	cmd, run := c.Command, c.Run
	if cmd == "" {
		cmd = "nvidia-smi"
	}
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}
	}
	out, err := run(ctx, cmd, "-q", "-x")
	if err != nil {
		return nil, fmt.Errorf("%s -q -x: %w", cmd, err)
	}
	readings, err := ParseXML(out)
	if err != nil {
		return nil, err
	}
	gpus := make(map[string]fitness.Health, len(readings))
	for _, r := range readings {
		gpus[r.Name] = Ratios(r, c.Params).Health
	}
	return gpus, nil
}
