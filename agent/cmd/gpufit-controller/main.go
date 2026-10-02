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
//
// Modes: "enforce" applies the above. "observe" (the default) makes the same
// decisions but changes nothing that affects scheduling: it records what it
// would do (events prefixed "observe mode, not applied", the status ConfigMap)
// and removes anything it applied earlier in enforce mode.
// Either way it writes ConfigMap gpufit-status (key status.json), which
// `kubectl gpufit status` reads.
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
	policyv1 "k8s.io/api/policy/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	applyresourcev1 "k8s.io/client-go/applyconfigurations/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/praveen9161/gpufit/agent/internal/pools"
	"github.com/praveen9161/gpufit/agent/internal/report"
	"github.com/praveen9161/gpufit/agent/internal/status"
)

const (
	manager       = "gpufit-controller"
	poolLabel     = "gpufit.io/pool"
	reasonAnn     = "gpufit.io/pool-reason"
	nodeLabel     = "gpufit.io/node"
	managedBy     = "app.kubernetes.io/managed-by"
	unfitTraining = "gpufit.io/unfit-training"
	needsRepair   = "gpufit.io/needs-repair"

	workloadLabel = "gpufit.io/workload"
	rescueLabel   = "gpufit.io/rescue"

	modeObserve = status.Observe
	modeEnforce = status.Enforce
)

type controller struct {
	client    kubernetes.Interface
	tracker   *pools.Tracker
	namespace string
	scoresCM  string
	statusCM  string
	driver    string
	mode      string

	// Savings report: training jobs on degraded GPUs, accumulated between reconciles.
	report   *report.Summary
	lastTick time.Time
	price    float64
	trainMin int

	// Rescue: move opted-in training jobs off GPUs that left the training pool.
	rescue      bool
	rescueGrace time.Duration
	degradedAt  map[string]time.Time // job key -> when it was first seen on a degraded GPU
}

// trainingJob is a training job's GPUs plus what's needed to move it.
type trainingJob struct {
	report.Job
	Pods   []*corev1.Pod
	Rescue bool // pods opted in with gpufit.io/rescue=true
}

func main() {
	cfg := pools.DefaultConfig
	c := controller{}
	flag.StringVar(&c.namespace, "namespace", envOr("POD_NAMESPACE", "dra-example-driver"), "namespace of the scores ConfigMap")
	flag.StringVar(&c.scoresCM, "scores-configmap", "gpufit-scores", "ConfigMap the agents write scores to")
	flag.StringVar(&c.statusCM, "status-configmap", "gpufit-status", "ConfigMap the controller writes its view to (read by kubectl gpufit status)")
	flag.StringVar(&c.driver, "driver", "gpu.example.com", "DRA driver whose devices get tainted")
	flag.StringVar(&c.mode, "mode", modeObserve, "observe (decide and report, change nothing) or enforce (apply pools and taints)")
	flag.IntVar(&cfg.TrainingMin, "training-min", cfg.TrainingMin, "node leaves the training pool if any GPU's training score is below this")
	flag.IntVar(&cfg.RepairMin, "repair-min", cfg.RepairMin, "GPU needs repair if its inference score is below this")
	flag.DurationVar(&cfg.DegradeAfter, "degrade-after", cfg.DegradeAfter, "how long a worse state must hold before it's applied")
	flag.DurationVar(&cfg.RecoverAfter, "recover-after", cfg.RecoverAfter, "how long a better state must hold before it's applied")
	interval := flag.Duration("interval", 5*time.Second, "how often to reconcile")
	flag.Float64Var(&c.price, "price-per-gpu-hour", 0, "GPU price for the savings report ($); 0 = report GPU-hours only")
	metricsAddr := flag.String("metrics-addr", ":8080", "address for Prometheus /metrics (empty disables)")
	flag.BoolVar(&c.rescue, "rescue", false, "enforce mode: restart opted-in training jobs (pod label gpufit.io/rescue=true) whose GPU left the training pool")
	flag.DurationVar(&c.rescueGrace, "rescue-grace", 2*time.Minute, "how long a job must sit on a degraded GPU before it's moved")
	cleanup := flag.Bool("cleanup", false, "remove all pools and taints this controller applied, then exit (helm uninstall runs it)")
	stopDeploy := flag.String("stop-deployment", "", "with --cleanup: first scale this controller Deployment (in --namespace) to 0 and wait, so it can't re-apply taints")
	flag.Parse()
	if c.mode != modeObserve && c.mode != modeEnforce {
		log.Fatalf("--mode must be %s or %s, got %q", modeObserve, modeEnforce, c.mode)
	}

	client, err := kubeClient()
	if err != nil {
		log.Fatal(err)
	}
	if *cleanup {
		if *stopDeploy != "" {
			if err := stopController(context.Background(), client, c.namespace, *stopDeploy, 90*time.Second); err != nil {
				log.Fatal(err)
			}
		}
		if err := cleanupAll(context.Background(), client); err != nil {
			log.Fatal(err)
		}
		return
	}
	c.client, c.tracker, c.trainMin = client, pools.NewTracker(cfg), cfg.TrainingMin
	c.report = c.loadReport(context.Background(), time.Now())
	serveMetrics(*metricsAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("gpufit-controller: mode=%s rescue=%t (grace %s) training-min=%d repair-min=%d degrade-after=%s recover-after=%s driver=%s",
		c.mode, c.rescue, c.rescueGrace, cfg.TrainingMin, cfg.RepairMin, cfg.DegradeAfter, cfg.RecoverAfter, c.driver)

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

	st := status.Status{Mode: c.mode, Updated: now.UTC(), Nodes: map[string]status.Node{}}
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
		var err error
		if c.mode == modeEnforce {
			err = c.apply(ctx, node, d)
		} else {
			err = c.observeOnly(ctx, node, d)
		}
		if err != nil {
			log.Printf("ERROR: node %s: %v", node, err)
		}
		st.Nodes[node] = status.Node{Pool: d.Pool, Reason: d.PoolReason, Repair: d.Repair, Applied: c.mode == modeEnforce, Scores: scores}
	}
	jobs, err := c.trainingJobs(ctx)
	if err != nil {
		log.Printf("WARN: training jobs: %v", err)
	}
	c.updateReport(now, st.Nodes, jobs)
	if c.mode == modeEnforce && c.rescue {
		c.rescueJobs(ctx, now, st.Nodes, jobs)
	}
	st.Report = c.report
	updateMetrics(st)
	return c.writeStatus(ctx, st)
}

// loadReport continues the totals saved in gpufit-status, or starts a new report.
func (c *controller) loadReport(ctx context.Context, now time.Time) *report.Summary {
	r := &report.Summary{Since: now.UTC(), Incidents: map[string]*report.Incident{}}
	if cm, err := c.client.CoreV1().ConfigMaps(c.namespace).Get(ctx, c.statusCM, metav1.GetOptions{}); err == nil {
		var st status.Status
		if json.Unmarshal([]byte(cm.Data[status.ConfigMapKey]), &st) == nil && st.Report != nil {
			r = st.Report
			log.Printf("report: continuing totals since %s", r.Since.Format(time.RFC3339))
		}
	}
	r.PricePerGPUHour = c.price
	return r
}

// updateReport accounts the time since the last reconcile to the training jobs
// that hold GPUs right now. Gaps (controller down) are not counted.
func (c *controller) updateReport(now time.Time, nodes map[string]status.Node, jobs []trainingJob) {
	if c.report == nil {
		c.report = &report.Summary{Since: now.UTC()}
	}
	dt := now.Sub(c.lastTick)
	c.lastTick = now
	if dt <= 0 || dt > 2*time.Minute {
		return // first tick, or a gap we can't vouch for
	}
	score := func(node, gpu string) (int, bool) {
		sc, ok := nodes[node].Scores[gpu]["training"]
		return sc, ok
	}
	plain := make([]report.Job, 0, len(jobs))
	for _, j := range jobs {
		plain = append(plain, j.Job)
	}
	c.report.Add(now.UTC(), dt, plain, score, c.trainMin)
}

// rescueJobs evicts every pod of an opted-in training job that has held a GPU
// outside the training pool for longer than the grace period. The Job
// recreates the pods; the device taint keeps them off the degraded GPU, and
// the job resumes from its own checkpoint. Eviction (not delete) respects
// PodDisruptionBudgets and marks the pods DisruptionTarget, which a Job's
// podFailurePolicy can ignore so the move doesn't count as a failure.
func (c *controller) rescueJobs(ctx context.Context, now time.Time, nodes map[string]status.Node, jobs []trainingJob) {
	if c.degradedAt == nil {
		c.degradedAt = map[string]time.Time{}
	}
	seen := map[string]bool{}
	for _, j := range jobs {
		if !j.Rescue {
			continue
		}
		from := ""
		for _, d := range j.Devices {
			if n, ok := nodes[d.Node]; ok && n.Applied && n.Pool != pools.Training {
				from = d.Node + "/" + d.Name
				break
			}
		}
		if from == "" {
			continue
		}
		seen[j.Key] = true
		first, ok := c.degradedAt[j.Key]
		if !ok {
			c.degradedAt[j.Key] = now
			log.Printf("rescue: %s holds %s, on a node that left the training pool; moving it after %s", j.Key, from, c.rescueGrace)
			continue
		}
		if now.Sub(first) < c.rescueGrace {
			continue
		}
		evicted := 0
		for _, p := range j.Pods {
			err := c.client.CoreV1().Pods(p.Namespace).EvictV1(ctx, &policyv1.Eviction{
				ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace},
			})
			if err != nil {
				log.Printf("ERROR: rescue: evict %s/%s: %v", p.Namespace, p.Name, err)
				continue
			}
			evicted++
		}
		log.Printf("rescue: %s moved off %s's node (%d pods evicted)", j.Key, from, evicted)
		c.report.AddRescue(report.Rescue{Job: j.Key, From: from, Pods: evicted, Time: now.UTC()})
		mRescues.Inc()
		delete(c.degradedAt, j.Key)
	}
	for k := range c.degradedAt {
		if !seen[k] {
			delete(c.degradedAt, k) // recovered or finished before the grace period ran out
		}
	}
}

// trainingJobs groups the GPUs held by training pods by the job that owns the
// pods. A claim is training work if it, or the pod using it, is labelled
// gpufit.io/workload=training (gpufit-claim sets it on claims).
func (c *controller) trainingJobs(ctx context.Context) ([]trainingJob, error) {
	claims, err := c.client.ResourceV1().ResourceClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list claims: %w", err)
	}
	pods, err := c.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	podByName := map[string]*corev1.Pod{}
	for i := range pods.Items {
		p := &pods.Items[i]
		podByName[p.Namespace+"/"+p.Name] = p
	}
	jobs := map[string]*trainingJob{}
	for _, cl := range claims.Items {
		if cl.Status.Allocation == nil || len(cl.Status.ReservedFor) == 0 {
			continue
		}
		pod := podByName[cl.Namespace+"/"+cl.Status.ReservedFor[0].Name]
		if pod != nil && (pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed) {
			continue // terminating (e.g. just rescued) or finished: no longer training here
		}
		workload := cl.Labels[workloadLabel]
		if workload == "" && pod != nil {
			workload = pod.Labels[workloadLabel]
		}
		if workload != "training" {
			continue
		}
		key := cl.Namespace + "/Claim/" + cl.Name
		if pod != nil {
			key = cl.Namespace + "/Pod/" + pod.Name
			if o := metav1.GetControllerOf(pod); o != nil {
				key = cl.Namespace + "/" + o.Kind + "/" + o.Name
			}
		}
		j, ok := jobs[key]
		if !ok {
			j = &trainingJob{Job: report.Job{Key: key}}
			jobs[key] = j
		}
		if pod != nil && !containsPod(j.Pods, pod) {
			j.Pods = append(j.Pods, pod)
			if pod.Labels[rescueLabel] == "true" {
				j.Rescue = true
			}
		}
		for _, res := range cl.Status.Allocation.Devices.Results {
			if res.Driver == c.driver {
				j.Devices = append(j.Devices, report.Device{Node: res.Pool, Name: res.Device})
			}
		}
	}
	out := make([]trainingJob, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, *j)
	}
	return out, nil
}

func containsPod(pods []*corev1.Pod, p *corev1.Pod) bool {
	for _, q := range pods {
		if q.Namespace == p.Namespace && q.Name == p.Name {
			return true
		}
	}
	return false
}

// observeOnly reports what enforce mode would do and removes anything this
// controller applied before (e.g. after switching from enforce to observe).
func (c *controller) observeOnly(ctx context.Context, node string, d pools.Decision) error {
	n, err := c.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node: %w", err)
	}
	for _, change := range d.Changes {
		msg := "observe mode, not applied: " + change
		log.Printf("node %s: %s", node, msg)
		c.event(ctx, n, msg)
	}
	if _, labelled := n.Labels[poolLabel]; labelled {
		// Applying an empty configuration with our field manager drops the
		// label and annotation we own, and nothing else.
		if _, err := c.client.CoreV1().Nodes().Apply(ctx, applycorev1.Node(node),
			metav1.ApplyOptions{FieldManager: manager, Force: true}); err != nil {
			return fmt.Errorf("unlabel node: %w", err)
		}
		log.Printf("node %s: removed pool label (observe mode)", node)
	}
	return c.removeRules(ctx, node, nil)
}

func (c *controller) writeStatus(ctx context.Context, st status.Status) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = c.client.CoreV1().ConfigMaps(c.namespace).Apply(ctx,
		applycorev1.ConfigMap(c.statusCM, c.namespace).WithData(map[string]string{status.ConfigMapKey: string(raw)}),
		metav1.ApplyOptions{FieldManager: manager, Force: true})
	if err != nil {
		return fmt.Errorf("write status: %w", err)
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
	return c.removeRules(ctx, node, want)
}

// stopController scales the controller Deployment to 0 and waits until its pods
// are gone; a controller still running would re-apply what cleanup removes.
func stopController(ctx context.Context, client kubernetes.Interface, ns, name string, timeout time.Duration) error {
	d, err := client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get deployment %s: %w", name, err)
	}
	if _, err := client.AppsV1().Deployments(ns).Patch(ctx, name, types.MergePatchType,
		[]byte(`{"spec":{"replicas":0}}`), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("scale %s to 0: %w", name, err)
	}
	selector := metav1.FormatLabelSelector(d.Spec.Selector)
	deadline := time.Now().Add(timeout)
	for {
		pods, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return fmt.Errorf("list controller pods: %w", err)
		}
		if len(pods.Items) == 0 {
			log.Printf("cleanup: controller %s/%s stopped", ns, name)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("controller %s/%s still has %d pods after %s", ns, name, len(pods.Items), timeout)
		}
		time.Sleep(time.Second)
	}
}

// cleanupAll removes everything enforce mode applies: this controller's
// DeviceTaintRules and the pool label/annotation on every node. Without it,
// uninstalling GPUFit would leave degraded nodes out of training for good.
func cleanupAll(ctx context.Context, client kubernetes.Interface) error {
	rules, err := client.ResourceV1().DeviceTaintRules().List(ctx, metav1.ListOptions{LabelSelector: managedBy + "=" + manager})
	if err != nil {
		return fmt.Errorf("list rules: %w", err)
	}
	for _, r := range rules.Items {
		if err := client.ResourceV1().DeviceTaintRules().Delete(ctx, r.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s: %w", r.Name, err)
		}
		log.Printf("cleanup: removed %s", r.Name)
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: poolLabel})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	for _, n := range nodes.Items {
		// An empty apply with our field manager drops only the fields we own.
		if _, err := client.CoreV1().Nodes().Apply(ctx, applycorev1.Node(n.Name),
			metav1.ApplyOptions{FieldManager: manager, Force: true}); err != nil {
			return fmt.Errorf("unlabel node %s: %w", n.Name, err)
		}
		log.Printf("cleanup: removed pool label from node %s", n.Name)
	}
	return nil
}

// removeRules deletes this controller's rules for the node except those in keep.
func (c *controller) removeRules(ctx context.Context, node string, keep map[string]*applyresourcev1.DeviceTaintRuleApplyConfiguration) error {
	existing, err := c.client.ResourceV1().DeviceTaintRules().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", managedBy, manager, nodeLabel, node),
	})
	if err != nil {
		return fmt.Errorf("list rules: %w", err)
	}
	for _, r := range existing.Items {
		if _, ok := keep[r.Name]; !ok {
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
