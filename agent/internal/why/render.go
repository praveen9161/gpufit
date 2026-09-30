package why

import (
	"fmt"
	"io"
	"strings"
)

// Render prints a report for humans, e.g.
//
//	claim gpus (train-small-gpus-x7k2p): NOT allocated
//	  option gpus/min95: needs 2, 0 usable
//	    in use (4): gpu-0, gpu-1, gpu-2, gpu-3 -> gpufit-lab/train-big
//	    selector false (1): gpu-5 (trainingScore 70)
//	    tainted, not tolerated (3): gpu-4, gpu-6, gpu-7 (gpufit.io/unfit-training)
func (r ClaimReport) Render(w io.Writer) {
	name := r.PodClaim
	if r.Claim != "" {
		name += " (" + r.Claim + ")"
	}
	if len(r.Allocated) > 0 {
		fmt.Fprintf(w, "claim %s: allocated %s\n", name, strings.Join(r.Allocated, ", "))
		return
	}
	fmt.Fprintf(w, "claim %s: NOT allocated\n", name)
	if len(r.Options) > 1 {
		fmt.Fprintf(w, "  options are tried in order; the first that fits wins, none fits right now:\n")
	}
	var prev *OptionReport
	for i := range r.Options {
		o := r.Options[i]
		need := fmt.Sprint(o.Need)
		if o.Need < 0 {
			need = "all"
		}
		mark := "✗"
		if o.Fits() {
			mark = "✓"
		}
		fmt.Fprintf(w, "  %s option %s: needs %s, %d usable", mark, o.Name, need, o.Count(Available))
		if len(o.Selectors) > 0 {
			fmt.Fprintf(w, "   [%s]", strings.Join(o.Selectors, " && "))
		}
		fmt.Fprintln(w)
		if o.Error != "" {
			fmt.Fprintf(w, "      error: %s\n", o.Error)
			continue
		}
		if prev != nil && sameVerdicts(*prev, o) {
			fmt.Fprintln(w, "      (same GPUs as the option above)")
			continue
		}
		prev = &r.Options[i]
		for _, reason := range []Reason{SelectorFalse, Tainted, InUse, Available} {
			groups := map[string][]string{} // detail -> devices, so "in use by X" groups nicely
			var order []string
			for _, v := range o.Verdicts {
				if v.Reason != reason {
					continue
				}
				d := v.Detail
				if reason == SelectorFalse {
					d = "" // details are per device, shown inline
				}
				if _, seen := groups[d]; !seen {
					order = append(order, d)
				}
				dev := v.Device
				if reason == SelectorFalse && v.Detail != "" {
					dev += " (" + v.Detail + ")"
				}
				groups[d] = append(groups[d], dev)
			}
			for _, d := range order {
				line := fmt.Sprintf("      %s (%d): %s", reason, len(groups[d]), strings.Join(groups[d], ", "))
				switch {
				case d != "" && reason == InUse:
					line += " -> " + d
				case d != "":
					line += " (" + d + ")"
				}
				fmt.Fprintln(w, line)
			}
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  note: %s\n", n)
	}
	if best := r.closest(); best != nil {
		fmt.Fprintf(w, "  summary: closest is %s with %d of %s usable; blocked by %s\n",
			best.Name, best.Count(Available), needString(best.Need), blockers(*best))
	}
}

// closest is the option with the most usable GPUs (the first one on ties).
func (r ClaimReport) closest() *OptionReport {
	var best *OptionReport
	for i := range r.Options {
		o := &r.Options[i]
		if o.Error == "" && (best == nil || o.Count(Available) > best.Count(Available)) {
			best = o
		}
	}
	return best
}

func blockers(o OptionReport) string {
	var parts []string
	for _, reason := range []Reason{InUse, Tainted, SelectorFalse} {
		if n := o.Count(reason); n > 0 {
			label := map[Reason]string{InUse: "busy", Tainted: "tainted", SelectorFalse: "failing the selector"}[reason]
			parts = append(parts, fmt.Sprintf("%d %s", n, label))
		}
	}
	if len(parts) == 0 {
		return "nothing (should fit; the scheduler may still be retrying)"
	}
	return strings.Join(parts, ", ")
}

func needString(n int) string {
	if n < 0 {
		return "all"
	}
	return fmt.Sprint(n)
}

func sameVerdicts(a, b OptionReport) bool {
	if len(a.Verdicts) != len(b.Verdicts) {
		return false
	}
	for i := range a.Verdicts {
		if a.Verdicts[i] != b.Verdicts[i] {
			return false
		}
	}
	return true
}
