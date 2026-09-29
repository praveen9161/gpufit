package collect

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func configMap(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "metrics"},
		Data:       data,
	}
}

func TestFakeConfigMapPrefersNodeKey(t *testing.T) {
	client := fake.NewClientset(configMap(map[string]string{
		"metrics.json": `{"gpu-0": {"compute": 1, "memory": 1, "nvlink": 1, "network": 1, "thermal": 1}}`,
		"node-a.json":  `{"gpu-0": {"compute": 0.5, "memory": 1, "nvlink": 1, "network": 1, "thermal": 1}}`,
	}))

	got, err := FakeConfigMap{Client: client, Namespace: "ns", Name: "metrics", Node: "node-a"}.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got["gpu-0"]["compute"] != 0.5 {
		t.Errorf("node-a: compute = %v, want 0.5 from node-a.json", got["gpu-0"]["compute"])
	}

	got, err = FakeConfigMap{Client: client, Namespace: "ns", Name: "metrics", Node: "node-b"}.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got["gpu-0"]["compute"] != 1 {
		t.Errorf("node-b: compute = %v, want 1 from metrics.json", got["gpu-0"]["compute"])
	}
}

func TestFakeConfigMapErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := (FakeConfigMap{Client: fake.NewClientset(), Namespace: "ns", Name: "metrics"}).Collect(ctx); err == nil {
		t.Error("want error for missing configmap")
	}
	noKey := fake.NewClientset(configMap(map[string]string{"other": "{}"}))
	if _, err := (FakeConfigMap{Client: noKey, Namespace: "ns", Name: "metrics", Node: "n"}).Collect(ctx); err == nil {
		t.Error("want error when neither key exists")
	}
	badJSON := fake.NewClientset(configMap(map[string]string{"metrics.json": "not json"}))
	if _, err := (FakeConfigMap{Client: badJSON, Namespace: "ns", Name: "metrics", Node: "n"}).Collect(ctx); err == nil {
		t.Error("want error for bad JSON")
	}
}
