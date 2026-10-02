// kubectl-gpufit is a kubectl plugin. Put it on your PATH, then:
//
//	kubectl gpufit status [-n gpufit-namespace]   # GPUs, scores, pools, what GPUFit does or would do
//	kubectl gpufit why <pod> [-n namespace]
//
// status reads ConfigMap gpufit-status, written by gpufit-controller.
//
// explains why a pod's DRA GPU claims aren't allocated: for every request
// option, which GPUs fail its selector (with the actual score), which are
// tainted (and why, e.g. node in the inference pool), which are in use and by
// whom, and which are free. Works with any DRA driver; the gpufit.io/* notes
// only appear when GPUFit is running.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/praveen9161/gpufit/agent/internal/status"
	"github.com/praveen9161/gpufit/agent/internal/why"
)

const usage = `usage:
  kubectl gpufit status [-n gpufit-namespace]   GPUs, scores, pools; what GPUFit does or would do (default namespace: gpufit)
  kubectl gpufit why <pod> [-n namespace]       why a pod's DRA GPU claims can't be allocated`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kubectl-gpufit:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	switch args[0] {
	case "why":
		return runWhy(args)
	case "status":
		return runStatus(args)
	case "-h", "--help", "help":
		fmt.Println(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n%s", args[0], usage)
}

// runStatus prints the controller's status record.
func runStatus(args []string) error {
	namespace := "gpufit"
	for i := 1; i < len(args); i++ {
		switch a := args[i]; a {
		case "-n", "--namespace":
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a value", a)
			}
			namespace, i = args[i+1], i+1
		default:
			return fmt.Errorf("unexpected argument %q\n%s", a, usage)
		}
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), "gpufit-status", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("no GPUFit status in namespace %q (is gpufit-controller running there? use -n): %w", namespace, err)
	}
	var st status.Status
	if err := json.Unmarshal([]byte(cm.Data[status.ConfigMapKey]), &st); err != nil {
		return fmt.Errorf("read status: %w", err)
	}
	st.Render(os.Stdout, time.Now())
	return nil
}

func runWhy(args []string) error {
	var pod, namespace string
	for i := 1; i < len(args); i++ { // flags may come before or after the pod name
		switch a := args[i]; a {
		case "-n", "--namespace":
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a value", a)
			}
			namespace, i = args[i+1], i+1
		case "-h", "--help":
			fmt.Println(usage)
			return nil
		default:
			pod = a
		}
	}
	if pod == "" {
		return fmt.Errorf("%s", usage)
	}

	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil)
	if namespace == "" {
		ns, _, err := loader.Namespace()
		if err != nil {
			return err
		}
		namespace = ns
	}
	cfg, err := loader.ClientConfig()
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()

	p, err := client.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return err
	}
	fmt.Printf("Pod %s/%s: %s\n", namespace, pod, p.Status.Phase)
	if msg := lastSchedulingMessage(ctx, client, p); msg != "" {
		fmt.Printf("Scheduler says: %s\n", msg)
	}
	if len(p.Spec.ResourceClaims) == 0 {
		fmt.Println("This pod has no DRA resource claims.")
		return nil
	}

	c, err := loadCluster(ctx, client)
	if err != nil {
		return err
	}
	fmt.Println()
	allAllocated := true
	for _, pc := range p.Spec.ResourceClaims {
		var claim *resourcev1.ResourceClaim
		if name := claimName(p, pc); name != "" {
			cl, err := client.ResourceV1().ResourceClaims(namespace).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				claim = cl
			}
		}
		r := why.Explain(ctx, c, pc.Name, claim)
		r.Render(os.Stdout)
		if len(r.Allocated) == 0 {
			allAllocated = false
		}
	}
	if allAllocated && p.Status.Phase == corev1.PodPending {
		fmt.Println("\nAll GPU claims are allocated; the pod is pending for another reason (see the scheduler message).")
	}
	return nil
}

// claimName resolves a pod's claim: a direct ResourceClaim, or the one the
// controller generated from a ResourceClaimTemplate (recorded in pod status).
func claimName(p *corev1.Pod, pc corev1.PodResourceClaim) string {
	if pc.ResourceClaimName != nil {
		return *pc.ResourceClaimName
	}
	for _, s := range p.Status.ResourceClaimStatuses {
		if s.Name == pc.Name && s.ResourceClaimName != nil {
			return *s.ResourceClaimName
		}
	}
	return ""
}

func loadCluster(ctx context.Context, client kubernetes.Interface) (why.Cluster, error) {
	c := why.Cluster{Classes: map[string]*resourcev1.DeviceClass{}, Nodes: map[string]*corev1.Node{}}
	classes, err := client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return c, fmt.Errorf("list device classes: %w", err)
	}
	for i := range classes.Items {
		c.Classes[classes.Items[i].Name] = &classes.Items[i]
	}
	slices, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return c, fmt.Errorf("list resource slices: %w", err)
	}
	c.Slices = slices.Items
	sort.Slice(c.Slices, func(i, j int) bool { return c.Slices[i].Name < c.Slices[j].Name })
	if rules, err := client.ResourceV1().DeviceTaintRules().List(ctx, metav1.ListOptions{}); err == nil {
		c.TaintRules = rules.Items // optional: older clusters don't serve DeviceTaintRules
	}
	claims, err := client.ResourceV1().ResourceClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return c, fmt.Errorf("list resource claims: %w", err)
	}
	c.Claims = claims.Items
	if nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err == nil {
		for i := range nodes.Items {
			c.Nodes[nodes.Items[i].Name] = &nodes.Items[i]
		}
	}
	return c, nil
}

func lastSchedulingMessage(ctx context.Context, client kubernetes.Interface, p *corev1.Pod) string {
	evs, err := client.CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.uid=%s,reason=FailedScheduling", p.Name, p.UID),
	})
	if err != nil || len(evs.Items) == 0 {
		return ""
	}
	last := evs.Items[0]
	for _, e := range evs.Items[1:] {
		if e.LastTimestamp.After(last.LastTimestamp.Time) || e.EventTime.After(last.EventTime.Time) {
			last = e
		}
	}
	return last.Message
}
