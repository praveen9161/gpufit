package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/praveen9161/gpufit/agent/internal/collect"
	"github.com/praveen9161/gpufit/agent/internal/fitness"
)

// runAgent is "gpufit-agent run": the long-running per-node loop.
//
// Every interval it collects health for this node's GPUs, scores them, and
// server-side-applies key "<node>.json" of the scores ConfigMap. Each node's
// agent uses its own field manager, so agents on different nodes own different
// keys and never overwrite each other. The DRA driver on the node reads that
// key and publishes the scores as device attributes.
func runAgent(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	node := fs.String("node", os.Getenv("NODE_NAME"), "this node's name (env NODE_NAME)")
	namespace := fs.String("namespace", envOr("POD_NAMESPACE", "dra-example-driver"), "namespace of the ConfigMaps (env POD_NAMESPACE)")
	scoresName := fs.String("scores-configmap", "gpufit-scores", "ConfigMap the DRA driver reads scores from")
	collector := fs.String("collector", "fake", "where health comes from: fake | nvidia-smi")
	fakeName := fs.String("fake-configmap", "gpufit-fake-metrics", "ConfigMap with fake health ratios (collector=fake)")
	nvidiaSMI := fs.String("nvidia-smi", "nvidia-smi", "nvidia-smi binary (collector=nvidia-smi)")
	exponent := fs.Float64("power-exponent", collect.DefaultParams.PowerExponent, "compute ~ power_ratio^exponent (collector=nvidia-smi)")
	window := fs.Duration("window", -1, "report the worst value over this long (default: 10m for nvidia-smi, off for fake)")
	interval := fs.Duration("interval", 5*time.Second, "how often to collect and publish")
	fs.Parse(args)

	if *node == "" {
		return fmt.Errorf("-node (or env NODE_NAME) is required")
	}
	client, err := kubeClient()
	if err != nil {
		return err
	}

	var c collect.Collector
	defaultWindow := time.Duration(0)
	switch *collector {
	case "fake":
		c = collect.FakeConfigMap{Client: client, Namespace: *namespace, Name: *fakeName, Node: *node}
	case "nvidia-smi":
		p := collect.DefaultParams
		p.PowerExponent = *exponent
		c = collect.NvidiaSMI{Command: *nvidiaSMI, Params: p}
		defaultWindow = 10 * time.Minute
		log.Printf("note: nvidia-smi collector v1 does not measure nvlink or network; both are reported as 1.0")
	default:
		return fmt.Errorf("unknown -collector %q, want fake or nvidia-smi", *collector)
	}
	if *window < 0 {
		*window = defaultWindow
	}
	if *window > 0 {
		c = &collect.Windowed{Inner: c, Window: *window}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("gpufit-agent on node %s: collector=%s, window=%s, writing %s/%s key %s.json every %s",
		*node, *collector, *window, *namespace, *scoresName, *node, *interval)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	var last map[string]map[string]fitness.Result
	for {
		results, err := tick(ctx, client, c, *namespace, *scoresName, *node)
		if err != nil {
			log.Printf("ERROR: %v", err)
		} else {
			logChanges(last, results)
			last = results
		}

		select {
		case <-ctx.Done():
			log.Printf("shutting down")
			return nil
		case <-ticker.C:
		}
	}
}

// tick does one collect -> score -> publish round.
func tick(ctx context.Context, client kubernetes.Interface, c collect.Collector, namespace, name, node string) (map[string]map[string]fitness.Result, error) {
	health, err := c.Collect(ctx)
	if err != nil {
		return nil, fmt.Errorf("collect: %w", err)
	}
	results, err := fitness.ScoreAll(health)
	if err != nil {
		return nil, fmt.Errorf("score: %w", err)
	}
	raw, err := json.Marshal(fitness.ScoreValues(results))
	if err != nil {
		return nil, err
	}

	// Server-side apply is idempotent: re-applying unchanged scores is a
	// no-op, and a deleted ConfigMap or key comes back on the next tick.
	cm := applycorev1.ConfigMap(name, namespace).WithData(map[string]string{node + ".json": string(raw)})
	_, err = client.CoreV1().ConfigMaps(namespace).Apply(ctx, cm, metav1.ApplyOptions{
		FieldManager: "gpufit-agent-" + node,
		Force:        true,
	})
	if err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	return results, nil
}

// logChanges prints one line per GPU whose scores changed, e.g.
//
//	gpu-3: training 100 -> 0 (nvlink 0.60 < floor 0.90), finetune 100 -> 93, inference 100
func logChanges(before, after map[string]map[string]fitness.Result) {
	gpus := make([]string, 0, len(after))
	for g := range after {
		gpus = append(gpus, g)
	}
	sort.Strings(gpus)

	for _, g := range gpus {
		var parts []string
		changed := before == nil
		for _, p := range fitness.DefaultProfiles {
			r := after[g][p.Name]
			part := fmt.Sprintf("%s %d", p.Name, r.Score)
			if old, ok := before[g][p.Name]; ok && old.Score != r.Score {
				part = fmt.Sprintf("%s %d -> %d", p.Name, old.Score, r.Score)
				changed = true
			}
			if r.Reason != "healthy" {
				part += " (" + r.Reason + ")"
			}
			parts = append(parts, part)
		}
		if changed {
			log.Printf("%s: %s", g, strings.Join(parts, ", "))
		}
	}
}

// kubeClient uses the pod's service account inside a cluster, or the normal
// kubeconfig (~/.kube/config, $KUBECONFIG) when run on a laptop.
func kubeClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("no in-cluster config and no kubeconfig: %w", err)
		}
	}
	return kubernetes.NewForConfig(cfg)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
