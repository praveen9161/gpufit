package why

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

const (
	driver = "gpu.example.com"
	node   = "worker"
)

// The node-pools demo, step 4: gpu-5 is a straggler (training 70), gpu-0..3
// run train-big, gpu-4 runs infer-1, and the node is in the inference pool.
func cluster(withPoolTaint bool) Cluster {
	var devs []resourcev1.Device
	for i := 0; i < 8; i++ {
		score := int64(100)
		if i == 5 {
			score = 70
		}
		devs = append(devs, resourcev1.Device{
			Name:       fmt.Sprintf("gpu-%d", i),
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"gpufit.io/trainingScore": {IntValue: ptr.To(score)}},
		})
	}
	c := Cluster{
		Classes: map[string]*resourcev1.DeviceClass{driver: {
			ObjectMeta: metav1.ObjectMeta{Name: driver},
			Spec: resourcev1.DeviceClassSpec{Selectors: []resourcev1.DeviceSelector{
				{CEL: &resourcev1.CELDeviceSelector{Expression: `device.driver == "gpu.example.com"`}},
			}},
		}},
		Slices: []resourcev1.ResourceSlice{{Spec: resourcev1.ResourceSliceSpec{
			Driver: driver, NodeName: ptr.To(node), Pool: resourcev1.ResourcePool{Name: node}, Devices: devs,
		}}},
		Claims: []resourcev1.ResourceClaim{
			allocated("train-big", "gpu-0", "gpu-1", "gpu-2", "gpu-3"),
			allocated("infer-1", "gpu-4"),
		},
		Nodes: map[string]*corev1.Node{node: {ObjectMeta: metav1.ObjectMeta{
			Name:        node,
			Labels:      map[string]string{"gpufit.io/pool": "inference"},
			Annotations: map[string]string{"gpufit.io/pool-reason": "gpu-5 training 70 < 80"},
		}}},
	}
	if withPoolTaint {
		c.TaintRules = []resourcev1.DeviceTaintRule{{Spec: resourcev1.DeviceTaintRuleSpec{
			DeviceSelector: &resourcev1.DeviceTaintSelector{Driver: ptr.To(driver), Pool: ptr.To(node)},
			Taint:          resourcev1.DeviceTaint{Key: "gpufit.io/unfit-training", Effect: resourcev1.DeviceTaintEffectNoSchedule},
		}}}
	}
	return c
}

func allocated(pod string, devices ...string) resourcev1.ResourceClaim {
	cl := resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "lab", Name: pod + "-gpus", UID: k8stypes.UID("uid-" + pod)}}
	var res []resourcev1.DeviceRequestAllocationResult
	for _, d := range devices {
		res = append(res, resourcev1.DeviceRequestAllocationResult{Request: "gpus", Driver: driver, Pool: node, Device: d})
	}
	cl.Status.Allocation = &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{Results: res}}
	cl.Status.ReservedFor = []resourcev1.ResourceClaimConsumerReference{{Resource: "pods", Name: pod}}
	return cl
}

// pending is a gpufit-claim style training claim: 2 GPUs, min95 -> min90 -> min80.
func pending(tolerateUnfitTraining bool) *resourcev1.ResourceClaim {
	var opts []resourcev1.DeviceSubRequest
	for _, t := range []int{95, 90, 80} {
		o := resourcev1.DeviceSubRequest{
			Name: fmt.Sprintf("min%d", t), DeviceClassName: driver,
			AllocationMode: resourcev1.DeviceAllocationModeExactCount, Count: 2,
			Selectors: []resourcev1.DeviceSelector{{CEL: &resourcev1.CELDeviceSelector{
				Expression: fmt.Sprintf(`device.attributes["gpufit.io"].trainingScore >= %d`, t)}}},
		}
		if tolerateUnfitTraining {
			o.Tolerations = []resourcev1.DeviceToleration{{Key: "gpufit.io/unfit-training", Operator: resourcev1.DeviceTolerationOpExists, Effect: resourcev1.DeviceTaintEffectNoSchedule}}
		}
		opts = append(opts, o)
	}
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "lab", Name: "train-small-gpus", UID: "uid-train-small"},
		Spec:       resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{Name: "gpus", FirstAvailable: opts}}}},
	}
}

func TestTrainingBlockedByInferencePool(t *testing.T) {
	r := Explain(context.Background(), cluster(true), "gpus", pending(false))
	if len(r.Options) != 3 {
		t.Fatalf("options = %d, want 3", len(r.Options))
	}
	min95 := r.Options[0]
	if min95.Fits() || min95.Count(SelectorFalse) != 1 || min95.Count(Tainted) != 7 {
		t.Errorf("min95: selector=%d tainted=%d available=%d, want 1/7/0",
			min95.Count(SelectorFalse), min95.Count(Tainted), min95.Count(Available))
	}
	var out strings.Builder
	r.Render(&out)
	for _, want := range []string{
		"NOT allocated",
		"gpu-5 (trainingScore 70)",
		"tainted, not tolerated (7)",
		"node worker is in the inference pool (gpu-5 training 70 < 80)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestWithoutTaintItFitsOnFreeGPUs(t *testing.T) {
	r := Explain(context.Background(), cluster(false), "gpus", pending(false))
	min95 := r.Options[0]
	// gpu-0..4 in use, gpu-5 too slow, gpu-6 and gpu-7 free -> fits.
	if !min95.Fits() || min95.Count(InUse) != 5 || min95.Count(Available) != 2 {
		t.Errorf("min95: in use=%d available=%d fits=%t, want 5/2/true", min95.Count(InUse), min95.Count(Available), min95.Fits())
	}
	var out strings.Builder
	r.Render(&out)
	if !strings.Contains(out.String(), "in use (4): gpu-0, gpu-1, gpu-2, gpu-3 -> lab/train-big") {
		t.Errorf("in-use grouping wrong:\n%s", out.String())
	}
}

func TestTolerationLetsInferenceThrough(t *testing.T) {
	r := Explain(context.Background(), cluster(true), "gpus", pending(true))
	if r.Options[0].Count(Tainted) != 0 || !r.Options[0].Fits() {
		t.Errorf("tolerating claim still blocked: tainted=%d", r.Options[0].Count(Tainted))
	}
}

func TestAllocatedAndMissing(t *testing.T) {
	c := cluster(false)
	cl := c.Claims[0]
	if r := Explain(context.Background(), c, "gpus", &cl); len(r.Allocated) != 4 {
		t.Errorf("allocated claim: %+v", r)
	}
	if r := Explain(context.Background(), c, "gpus", nil); len(r.Notes) != 1 {
		t.Errorf("missing claim: %+v", r)
	}
	bad := pending(false)
	bad.Spec.Devices.Requests[0].FirstAvailable[0].DeviceClassName = "nope"
	if r := Explain(context.Background(), c, "gpus", bad); !strings.Contains(r.Options[0].Error, "not found") {
		t.Errorf("unknown class: %+v", r.Options[0])
	}
}
