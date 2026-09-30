package main

import (
	"context"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/praveen9161/gpufit/agent/internal/pools"
)

const (
	healthyNode = `{"gpu-0":{"training":100,"finetune":100,"inference":100},"gpu-1":{"training":100,"finetune":100,"inference":100}}`
	stragglerOn = `{"gpu-0":{"training":100,"finetune":100,"inference":100},"gpu-1":{"training":70,"finetune":88,"inference":85}}`
	brokenGPU   = `{"gpu-0":{"training":100,"finetune":100,"inference":100},"gpu-1":{"training":0,"finetune":0,"inference":0}}`
)

func setup(t *testing.T, data map[string]string) (*controller, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "gpufit-scores"}, Data: data},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
	)
	cfg := pools.Config{TrainingMin: 80, RepairMin: 50, DegradeAfter: time.Minute, RecoverAfter: 2 * time.Minute}
	return &controller{client: cs, tracker: pools.NewTracker(cfg), namespace: "ns", scoresCM: "gpufit-scores", driver: "gpu.example.com"}, cs
}

func setScores(t *testing.T, cs *fake.Clientset, data map[string]string) {
	t.Helper()
	cm, _ := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), "gpufit-scores", metav1.GetOptions{})
	cm.Data = data
	if _, err := cs.CoreV1().ConfigMaps("ns").Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func ruleNames(t *testing.T, cs *fake.Clientset) []string {
	t.Helper()
	list, err := cs.ResourceV1().DeviceTaintRules().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range list.Items {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return names
}

func pool(t *testing.T, cs *fake.Clientset, node string) string {
	t.Helper()
	n, err := cs.CoreV1().Nodes().Get(context.Background(), node, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return n.Labels[poolLabel]
}

func TestReconcileLifecycle(t *testing.T) {
	ctx, t0 := context.Background(), time.Now()
	c, cs := setup(t, map[string]string{"node-a.json": healthyNode, "node-b.json": healthyNode, "scores.json": stragglerOn})

	// Start: both healthy. scores.json is ignored (not a node).
	if err := c.reconcile(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if pool(t, cs, "node-a") != "training" || pool(t, cs, "node-b") != "training" || len(ruleNames(t, cs)) != 0 {
		t.Fatalf("start: a=%s b=%s rules=%v", pool(t, cs, "node-a"), pool(t, cs, "node-b"), ruleNames(t, cs))
	}

	// node-a gets a straggler and a broken GPU; applied only after DegradeAfter.
	setScores(t, cs, map[string]string{"node-a.json": brokenGPU, "node-b.json": healthyNode})
	c.reconcile(ctx, t0.Add(10*time.Second))
	if pool(t, cs, "node-a") != "training" {
		t.Fatalf("node-a moved before degrade-after")
	}
	c.reconcile(ctx, t0.Add(80*time.Second))
	want := []string{"gpufit-node-a-gpu-1-needs-repair", "gpufit-node-a-unfit-training"}
	if got := ruleNames(t, cs); pool(t, cs, "node-a") != "inference" || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("degraded: pool=%s rules=%v, want inference + %v", pool(t, cs, "node-a"), got, want)
	}
	if pool(t, cs, "node-b") != "training" {
		t.Errorf("node-b affected by node-a")
	}

	// The per-GPU rule targets exactly that device, NoSchedule.
	r, _ := cs.ResourceV1().DeviceTaintRules().Get(ctx, "gpufit-node-a-gpu-1-needs-repair", metav1.GetOptions{})
	sel := r.Spec.DeviceSelector
	if *sel.Driver != "gpu.example.com" || *sel.Pool != "node-a" || *sel.Device != "gpu-1" ||
		r.Spec.Taint.Key != needsRepair || r.Spec.Taint.Effect != "NoSchedule" {
		t.Errorf("repair rule = %+v / %+v", *sel, r.Spec.Taint)
	}

	// Events were recorded for the changes.
	evs, _ := cs.CoreV1().Events("default").List(ctx, metav1.ListOptions{})
	if len(evs.Items) < 2 {
		t.Errorf("events = %d, want at least 2 (pool change + needs repair)", len(evs.Items))
	}

	// Repaired: rules are removed only after RecoverAfter.
	setScores(t, cs, map[string]string{"node-a.json": healthyNode, "node-b.json": healthyNode})
	c.reconcile(ctx, t0.Add(90*time.Second))
	if len(ruleNames(t, cs)) != 2 {
		t.Fatalf("rules removed before recover-after")
	}
	c.reconcile(ctx, t0.Add(4*time.Minute))
	if pool(t, cs, "node-a") != "training" || len(ruleNames(t, cs)) != 0 {
		t.Fatalf("recovered: pool=%s rules=%v", pool(t, cs, "node-a"), ruleNames(t, cs))
	}
}
