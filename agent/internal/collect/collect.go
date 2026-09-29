// Package collect gets per-GPU health ratios from some source.
//
// A Collector is the only part of GPUFit that knows where health numbers come
// from. Everything after it (scoring, publishing) is the same for simulated
// and real GPUs.
package collect

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/praveen9161/gpufit/agent/internal/fitness"
)

// Collector returns the current health of every GPU on this node, keyed by
// the device name the DRA driver uses (e.g. "gpu-0").
type Collector interface {
	Collect(ctx context.Context) (map[string]fitness.Health, error)
}

// FakeConfigMap reads made-up health ratios from a ConfigMap, so a simulated
// GPU can be "degraded" with kubectl edit. It uses key "<node>.json" if
// present, otherwise "metrics.json" (same fake metrics for every node).
//
//	data:
//	  metrics.json: {"gpu-0": {"compute": 1.0, "memory": 1.0, ...}, ...}
type FakeConfigMap struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
	Node      string
}

func (f FakeConfigMap) Collect(ctx context.Context) (map[string]fitness.Health, error) {
	cm, err := f.Client.CoreV1().ConfigMaps(f.Namespace).Get(ctx, f.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	key := f.Node + ".json"
	raw, ok := cm.Data[key]
	if !ok {
		key = "metrics.json"
		raw, ok = cm.Data[key]
	}
	if !ok {
		return nil, fmt.Errorf("configmap %s/%s has neither %s.json nor metrics.json", f.Namespace, f.Name, f.Node)
	}
	var gpus map[string]fitness.Health
	if err := json.Unmarshal([]byte(raw), &gpus); err != nil {
		return nil, fmt.Errorf("configmap %s/%s key %s: %w", f.Namespace, f.Name, key, err)
	}
	return gpus, nil
}
