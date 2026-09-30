// gpufit-controller moves nodes between a training and an inference pool, and
// marks GPUs for repair, based on the per-node scores the agents publish.
// Design: docs/design/node-pools.md. Decisions: internal/pools.
//
// Every interval it reads ConfigMap gpufit-scores (keys "<node>.json"), and
// for each node applies:
//   - node label gpufit.io/pool=training|inference + annotation gpufit.io/pool-reason
//   - DeviceTaintRule gpufit-<node>-unfit-training (all GPUs of the node) while in the inference pool
//   - DeviceTaintRule gpufit-<node>-<gpu>-needs-repair per GPU that needs repair
//   - a Kubernetes Event on the Node for every change
//
// Device taints are NoSchedule: new claims avoid the GPUs, running jobs stay.
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

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	applyresourcev1 "k8s.io/client-go/applyconfigurations/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/praveen9161/gpufit/agent/internal/pools"
)

const (
	manager       = "gpufit-controller"
	poolLabel     = "gpufit.io/pool"
	reasonAnn     = "gpufit.io/pool-reason"
	nodeLabel     = "gpufit.io/node"
	managedBy     = "app.kubernetes.io/managed-by"
	unfitTraining = "gpufit.io/unfit-training"
	needsRepair   = "gpufit.io/needs-repair"
)

type controller struct {
	client    kubernetes.Interface
	tracker   *pools.Tracker
	namespace string
	scoresCM  string
	driver    string
}

func main() {
	cfg := pools.DefaultConfig
	c := controller{}
	flag.StringVar(&c.namespace, "namespace", envOr("POD_NAMESPACE", "dra-example-driver"), "namespace of the scores ConfigMap")
	flag.StringVar(&c.scoresCM, "scores-configmap", "gpufit-scores", "ConfigMap the agents write scores to")
	flag.StringVar(&c.driver, "driver", "gpu.example.com", "DRA driver whose devices get tainted")
	flag.IntVar(&cfg.TrainingMin, "training-min", cfg.TrainingMin, "node leaves the training pool if any GPU's training score is below this")
	flag.IntVar(&cfg.RepairMin, "repair-min", cfg.RepairMin, "GPU needs repair if its inference score is below this")
	flag.DurationVar(&cfg.DegradeAfter, "degrade-after", cfg.DegradeAfter, "how long a worse state must hold before it's applied")
	flag.DurationVar(&cfg.RecoverAfter, "recover-after", cfg.RecoverAfter, "how long a better state must hold before it's applied")
	interval := flag.Duration("interval", 5*time.Second, "how often to reconcile")
	flag.Parse()

	client, err := kubeClient()
	if err != nil {
		log.Fatal(err)
	}
	c.client, c.tracker = client, pools.NewTracker(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("gpufit-controller: training-min=%d repair-min=%d degrade-after=%s recover-after=%s driver=%s",
		cfg.TrainingMin, cfg.RepairMin, cfg.DegradeAfter, cfg.RecoverAfter, c.driver)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		if err := c.reconcile(ctx, time.Now()); err != nil {
			log.Printf("ERROR: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *controller) reconcile(ctx context.Context, now time.Time) error {
	cm, err := c.client.CoreV1().ConfigMaps(c.namespace).Get(ctx, c.scoresCM, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read scores: %w", err)
	}
	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		node, ok := strings.CutSuffix(key, ".json")
		if !ok || node == "scores" { // scores.json is the cluster-wide CLI fallback, not a node
			continue
		}
		var scores pools.Scores
		if err := json.Unmarshal([]byte(cm.Data[key]), &scores); err != nil {
			log.Printf("ERROR: %s: %v", key, err)
			continue
		}
		d := c.tracker.Observe(now, node, scores)
		if err := c.apply(ctx, node, d); err != nil {
			log.Printf("ERROR: node %s: %v", node, err)
		}
	}
	return nil
}

func (c *controller) apply(ctx context.Context, node string, d pools.Decision) error {
	opts := metav1.ApplyOptions{FieldManager: manager, Force: true}

	// 1. Node label + reason (what humans and node-level tools see).
	n, err := c.client.CoreV1().Nodes().Apply(ctx, applycorev1.Node(node).
		WithLabels(map[string]string{poolLabel: d.Pool}).
		WithAnnotations(map[string]string{reasonAnn: d.PoolReason}), opts)
	if err != nil {
		return fmt.Errorf("label node: %w", err)
	}
	for _, change := range d.Changes {
		log.Printf("node %s: %s", node, change)
		c.event(ctx, n, change)
	}

	// 2. Device taints (what the DRA scheduler sees).
	want := map[string]*applyresourcev1.DeviceTaintRuleApplyConfiguration{}
	if d.Pool == pools.Inference {
		name := fmt.Sprintf("gpufit-%s-unfit-training", node)
		want[name] = c.rule(name, node, "", unfitTraining)
	}
	for gpu := range d.Repair {
		name := fmt.Sprintf("gpufit-%s-%s-needs-repair", node, gpu)
		want[name] = c.rule(name, node, gpu, needsRepair)
	}
	for _, r := range want {
		if _, err := c.client.ResourceV1().DeviceTaintRules().Apply(ctx, r, opts); err != nil {
			return fmt.Errorf("apply %s: %w", *r.Name, err)
		}
	}

	// 3. Remove our rules for this node that are no longer wanted.
	existing, err := c.client.ResourceV1().DeviceTaintRules().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", managedBy, manager, nodeLabel, node),
	})
	if err != nil {
		return fmt.Errorf("list rules: %w", err)
	}
	for _, r := range existing.Items {
		if _, keep := want[r.Name]; !keep {
			if err := c.client.ResourceV1().DeviceTaintRules().Delete(ctx, r.Name, metav1.DeleteOptions{}); err != nil {
				return fmt.Errorf("delete %s: %w", r.Name, err)
			}
			log.Printf("node %s: removed %s", node, r.Name)
		}
	}
	return nil
}

// rule taints all GPUs of the node (gpu == "") or one GPU, NoSchedule.
func (c *controller) rule(name, node, gpu, key string) *applyresourcev1.DeviceTaintRuleApplyConfiguration {
	sel := applyresourcev1.DeviceTaintSelector().WithDriver(c.driver).WithPool(node)
	if gpu != "" {
		sel = sel.WithDevice(gpu)
	}
	return applyresourcev1.DeviceTaintRule(name).
		WithLabels(map[string]string{managedBy: manager, nodeLabel: node}).
		WithSpec(applyresourcev1.DeviceTaintRuleSpec().
			WithDeviceSelector(sel).
			WithTaint(applyresourcev1.DeviceTaint().WithKey(key).WithEffect(resourcev1.DeviceTaintEffectNoSchedule)))
}

// event records a change on the Node, visible in `kubectl describe node` and
// `kubectl get events`. Failures are logged, never fatal.
func (c *controller) event(ctx context.Context, n *corev1.Node, msg string) {
	typ, reason := corev1.EventTypeNormal, "GPUFitPoolChanged"
	if strings.Contains(msg, "needs repair") {
		typ, reason = corev1.EventTypeWarning, "GPUFitNeedsRepair"
	} else if strings.Contains(msg, "inference") {
		typ = corev1.EventTypeWarning
	}
	now := metav1.Now()
	_, err := c.client.CoreV1().Events(metav1.NamespaceDefault).Create(ctx, &corev1.Event{
		// Unique name like client-go's recorder: <object>.<hex timestamp>.
		ObjectMeta:     metav1.ObjectMeta{Name: fmt.Sprintf("%s.gpufit-%x", n.Name, now.UnixNano())},
		InvolvedObject: corev1.ObjectReference{Kind: "Node", APIVersion: "v1", Name: n.Name, UID: n.UID},
		Reason:         reason,
		Message:        msg,
		Type:           typ,
		Source:         corev1.EventSource{Component: manager},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}, metav1.CreateOptions{})
	if err != nil {
		log.Printf("WARN: event for %s: %v", n.Name, err)
	}
}

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
