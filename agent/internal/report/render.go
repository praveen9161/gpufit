package report

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"
)

// Render prints the report for `kubectl gpufit report`. mode is the
// controller's mode ("observe" or "enforce"), which changes what the numbers mean.
func (s *Summary) Render(w io.Writer, now time.Time, mode string) {
	fmt.Fprintf(w, "GPUFit savings report   since %s (%s)   mode: %s\n\n",
		s.Since.UTC().Format("2006-01-02 15:04 MST"), now.Sub(s.Since).Round(time.Minute), mode)
	pct := 0.0
	if s.TrainingGPUHours > 0 {
		pct = 100 * s.DegradedGPUHours / s.TrainingGPUHours
	}
	fmt.Fprintf(w, "  Training GPU-hours                %8.2f\n", s.TrainingGPUHours)
	fmt.Fprintf(w, "    in jobs holding a degraded GPU  %8.2f   (%.1f%%)\n", s.DegradedGPUHours, pct)
	fmt.Fprintf(w, "  Estimated waste (stragglers)      %8.2f GPU-hours", s.EstimatedWastedGPUHours)
	if s.PricePerGPUHour > 0 {
		fmt.Fprintf(w, "   ~ $%.2f at $%.2f/GPU-hour", s.EstimatedWastedDollars(), s.PricePerGPUHour)
	} else {
		fmt.Fprint(w, "   (set --price-per-gpu-hour for dollars)")
	}
	fmt.Fprintln(w)
	switch mode {
	case "observe":
		fmt.Fprintln(w, "\n  Observe mode: this is waste that enforcing GPUFit would keep new training jobs from.")
	default:
		fmt.Fprintln(w, "\n  Enforce mode: new training avoids degraded GPUs; waste here comes from jobs placed before a GPU degraded.")
	}

	if inc := s.SortedIncidents(); len(inc) > 0 {
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  JOB\tGPUS\tSLOWEST GPU\tSCORE\tDEGRADED FOR\tWASTED GPU-h")
		for _, i := range inc {
			fmt.Fprintf(tw, "  %s\t%d\t%s\t%d\t%s\t%.2f\n", i.Job, i.GPUs, i.Slowest, i.SlowestScore,
				i.LastSeen.Sub(i.FirstSeen).Round(time.Second), i.WastedGPUHours)
		}
		tw.Flush()
	}
	if len(s.Rescues) > 0 {
		fmt.Fprintf(w, "\n  Rescued training jobs (moved off degraded GPUs): %d\n", len(s.Rescues))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  TIME\tJOB\tFROM GPU\tPODS")
		for i := len(s.Rescues) - 1; i >= 0 && i >= len(s.Rescues)-10; i-- {
			r := s.Rescues[i]
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\n", r.Time.UTC().Format("15:04:05"), r.Job, r.From, r.Pods)
		}
		tw.Flush()
	}
	fmt.Fprintln(w, "\n  Speed is estimated from the training score (score/100). That overstates mild degradation:")
	fmt.Fprintln(w, "  on a real A10G a GPU scoring 50 still ran training at 0.84 (GPUFit session B).")
}
