package claims

import (
	"slices"
	"strings"
	"testing"
)

func TestTiers(t *testing.T) {
	for _, tc := range []struct {
		min    int
		prefer []int
		want   []int
	}{
		{80, []int{95, 90}, []int{95, 90, 80}},
		{80, []int{90, 95, 90}, []int{95, 90, 80}}, // sorted, deduplicated
		{92, []int{95, 90}, []int{95, 92}},         // 90 is below min, dropped
		{95, []int{95, 90}, []int{95}},             // nothing above min
		{80, nil, []int{80}},
	} {
		got, err := Tiers(tc.min, tc.prefer)
		if err != nil {
			t.Fatalf("Tiers(%d, %v): %v", tc.min, tc.prefer, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("Tiers(%d, %v) = %v, want %v", tc.min, tc.prefer, got, tc.want)
		}
	}
}

func TestTiersRejectsBadInput(t *testing.T) {
	if _, err := Tiers(101, nil); err == nil {
		t.Error("want error for min > 100")
	}
	if _, err := Tiers(80, []int{120}); err == nil {
		t.Error("want error for preferred score > 100")
	}
	if _, err := Tiers(10, []int{20, 30, 40, 50, 60, 70, 80, 90}); err == nil {
		t.Error("want error for more than 8 tiers")
	}
}

func TestWrite(t *testing.T) {
	var b strings.Builder
	err := Write(&b, Options{
		Namespace: "gpufit-lab", Workload: "training", Count: 2, Min: 80,
		Prefer: DefaultPrefer, DeviceClass: "gpu.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"name: training-min80-2gpu",
		"gpufit.io/min-score: \"80\"",
		"count: 2",
		`expression: device.attributes["gpufit.io"].trainingScore >= 95`,
		`expression: device.attributes["gpufit.io"].trainingScore >= 80`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Strictest option must come first.
	if strings.Index(out, "name: min95") > strings.Index(out, "name: min80") {
		t.Errorf("min95 should come before min80:\n%s", out)
	}
}

func TestWriteRejectsBadInput(t *testing.T) {
	base := Options{Namespace: "ns", Workload: "training", Count: 1, Min: 80, DeviceClass: "c"}
	for name, mutate := range map[string]func(*Options){
		"unknown workload": func(o *Options) { o.Workload = "gaming" },
		"zero count":       func(o *Options) { o.Count = 0 },
		"bad name":         func(o *Options) { o.Name = "Bad_Name" },
	} {
		o := base
		mutate(&o)
		if err := Write(&strings.Builder{}, o); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// Only non-training claims may land on nodes in the inference pool.
func TestTolerationsByWorkload(t *testing.T) {
	for workload, want := range map[string]bool{"training": false, "finetune": true, "inference": true} {
		var b strings.Builder
		if err := Write(&b, Options{Namespace: "ns", Workload: workload, Count: 1, Min: 80, Prefer: DefaultPrefer, DeviceClass: "c"}); err != nil {
			t.Fatal(err)
		}
		got := strings.Count(b.String(), "key: gpufit.io/unfit-training")
		if want && got != 3 { // one per firstAvailable option (95, 90, 80)
			t.Errorf("%s: %d tolerations, want 3", workload, got)
		}
		if !want && got != 0 {
			t.Errorf("%s: %d tolerations, want none", workload, got)
		}
	}
}
