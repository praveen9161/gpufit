package collect

import (
	"context"
	"math"
	"time"

	"github.com/praveen9161/gpufit/agent/internal/fitness"
)

// Windowed reports, per GPU and component, the WORST ratio any collection
// returned during the last Window. An idle GPU looks perfect (cool, never
// throttling), so one reading says little; the worst value over e.g. 10
// minutes includes the last time the GPU was under load.
//
// Not safe for concurrent use; the agent calls it from one loop.
type Windowed struct {
	Inner  Collector
	Window time.Duration
	Now    func() time.Time // default time.Now; tests replace it

	samples []sample
}

type sample struct {
	at   time.Time
	gpus map[string]fitness.Health
}

func (w *Windowed) Collect(ctx context.Context) (map[string]fitness.Health, error) {
	gpus, err := w.Inner.Collect(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	w.samples = append(w.samples, sample{at: now, gpus: gpus})

	// Drop samples older than the window (always keep the newest).
	keep := w.samples[:0]
	for _, s := range w.samples {
		if now.Sub(s.at) <= w.Window {
			keep = append(keep, s)
		}
	}
	w.samples = keep

	// Worst value per component. Only GPUs present now are reported, so a
	// removed GPU disappears immediately.
	out := make(map[string]fitness.Health, len(gpus))
	for gpu := range gpus {
		worst := fitness.Health{}
		for _, c := range fitness.Components {
			worst[c] = math.Inf(1)
		}
		for _, s := range w.samples {
			h, ok := s.gpus[gpu]
			if !ok {
				continue
			}
			for _, c := range fitness.Components {
				if v, ok := h[c]; ok {
					worst[c] = math.Min(worst[c], v)
				}
			}
		}
		for _, c := range fitness.Components {
			if math.IsInf(worst[c], 1) {
				delete(worst, c) // never reported; let scoring flag it
			}
		}
		out[gpu] = worst
	}
	return out, nil
}
