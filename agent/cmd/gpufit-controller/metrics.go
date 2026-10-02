package main

import (
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/praveen9161/gpufit/agent/internal/status"
)

// Prometheus metrics, refreshed from the status record after every reconcile.
// The report values are totals since report start (persisted across restarts),
// so they're gauges, not counters.
var (
	registry = prometheus.NewRegistry()

	mModeEnforce = gauge("gpufit_mode_enforce", "1 if GPUFit applies pools and taints, 0 in observe mode.")
	mScore       = gaugeVec("gpufit_gpu_score", "Fitness score (0-100) per GPU and workload.", "node", "gpu", "workload")
	mOutOfPool   = gaugeVec("gpufit_node_out_of_training_pool", "1 if the node is (or in observe mode would be) out of the training pool.", "node", "applied")
	mRepair      = gaugeVec("gpufit_gpus_needing_repair", "GPUs on the node unfit even for inference.", "node")
	mTrainHours  = gauge("gpufit_report_training_gpu_hours", "GPU-hours used by training jobs since report start.")
	mDegHours    = gauge("gpufit_report_degraded_training_gpu_hours", "GPU-hours of training jobs that held a degraded GPU.")
	mWasteHours  = gauge("gpufit_report_estimated_wasted_gpu_hours", "Estimated GPU-hours lost to stragglers (pessimistic, from scores).")
	mWasteDollar = gauge("gpufit_report_estimated_wasted_dollars", "Estimated waste in dollars at --price-per-gpu-hour.")
	mSince       = gauge("gpufit_report_start_timestamp_seconds", "When the report's totals started (Unix time).")
	mRescues     = counter("gpufit_rescues_total", "Training jobs moved off degraded GPUs by this controller process.")
)

func counter(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
	registry.MustRegister(c)
	return c
}

func gauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
	registry.MustRegister(g)
	return g
}

func gaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	registry.MustRegister(g)
	return g
}

func updateMetrics(st status.Status) {
	mModeEnforce.Set(map[bool]float64{true: 1, false: 0}[st.Mode == status.Enforce])
	mScore.Reset()
	mOutOfPool.Reset()
	mRepair.Reset()
	for node, n := range st.Nodes {
		for gpu, byWorkload := range n.Scores {
			for workload, sc := range byWorkload {
				mScore.WithLabelValues(node, gpu, workload).Set(float64(sc))
			}
		}
		out := 0.0
		if n.Pool != "training" {
			out = 1
		}
		mOutOfPool.WithLabelValues(node, map[bool]string{true: "true", false: "false"}[n.Applied]).Set(out)
		mRepair.WithLabelValues(node).Set(float64(len(n.Repair)))
	}
	if r := st.Report; r != nil {
		mTrainHours.Set(r.TrainingGPUHours)
		mDegHours.Set(r.DegradedGPUHours)
		mWasteHours.Set(r.EstimatedWastedGPUHours)
		mWasteDollar.Set(r.EstimatedWastedDollars())
		mSince.Set(float64(r.Since.Unix()))
	}
}

// serveMetrics exposes /metrics on addr ("" disables).
func serveMetrics(addr string) {
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	go func() {
		log.Printf("metrics on %s/metrics", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("ERROR: metrics server: %v", err)
		}
	}()
}
