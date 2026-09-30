// Package why explains why a pod's DRA resource claims can't be allocated.
//
// The scheduler only says "cannot allocate all claims". This package looks at
// every device the claim could use and sorts it into a reason, in the same
// order the question matters to a person:
//
//	wrong class -> selector false (e.g. score too low) -> tainted, not tolerated -> in use -> available
//
// Selectors are evaluated with k8s.io/dynamic-resource-allocation/cel, the
// same CEL code the scheduler uses, so "selector false" means what the
// scheduler thinks. Node-level constraints (node selectors, CPU/memory) are
// out of scope; the scheduler event covers those.
package why

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	dracel "k8s.io/dynamic-resource-allocation/cel"
)

// Cluster is the state the explanation is computed from.
type Cluster struct {
	Classes    map[string]*resourcev1.DeviceClass
	Slices     []resourcev1.ResourceSlice
	TaintRules []resourcev1.DeviceTaintRule
	Claims     []resourcev1.ResourceClaim // all claims, to see who uses which device
	Nodes      map[string]*corev1.Node    // for gpufit.io/pool-reason
}

// Reason a device can't be used for a request option.
type Reason string

const (
	WrongClass    Reason = "wrong device class"
	SelectorFalse Reason = "selector false"
	Tainted       Reason = "tainted, not tolerated"
	InUse         Reason = "in use"
	Available     Reason = "available"
)

// DeviceVerdict is one device's reason plus a short detail, e.g. "trainingScore 70".
type DeviceVerdict struct {
	Device string // "gpu-3" (pool/device if more than one pool)
	Reason Reason
	Detail string
}

// OptionReport explains one request option (the request itself, or one
// firstAvailable entry).
type OptionReport struct {
	Name      string   // "gpus" or "gpus/min95"
	Need      int      // devices needed (-1 = All)
	Selectors []string // CEL expressions from class + request
	Verdicts  []DeviceVerdict
	Error     string // e.g. unknown class, CEL compile error
}

func (o OptionReport) Count(r Reason) int {
	n := 0
	for _, v := range o.Verdicts {
		if v.Reason == r {
			n++
		}
	}
	return n
}

// Fits reports whether enough devices are available right now.
func (o OptionReport) Fits() bool {
	if o.Error != "" {
		return false
	}
	if o.Need < 0 {
		return o.Count(Available) > 0 && o.Count(Tainted)+o.Count(InUse) == 0
	}
	return o.Count(Available) >= o.Need
}

// ClaimReport explains one claim of the pod.
type ClaimReport struct {
	PodClaim  string // name in the pod spec
	Claim     string // ResourceClaim name ("" if not created yet)
	Allocated []string
	Options   []OptionReport
	Notes     []string // e.g. node pool reasons
}

// Explain analyses one claim.
func Explain(ctx context.Context, c Cluster, podClaim string, claim *resourcev1.ResourceClaim) ClaimReport {
	r := ClaimReport{PodClaim: podClaim}
	if claim == nil {
		r.Notes = append(r.Notes, "claim not created yet (template not found, or controller hasn't made it)")
		return r
	}
	r.Claim = claim.Name
	if a := claim.Status.Allocation; a != nil {
		for _, res := range a.Devices.Results {
			r.Allocated = append(r.Allocated, res.Device)
		}
		return r
	}

	inUse := usedBy(c.Claims, claim.UID)
	notes := map[string]bool{}
	for _, req := range claim.Spec.Devices.Requests {
		switch {
		case req.Exactly != nil:
			e := req.Exactly
			r.Options = append(r.Options, explainOption(ctx, c, req.Name, e.DeviceClassName, e.AllocationMode, e.Count, e.Selectors, e.Tolerations, inUse, notes))
		default:
			for _, sub := range req.FirstAvailable {
				r.Options = append(r.Options, explainOption(ctx, c, req.Name+"/"+sub.Name, sub.DeviceClassName, sub.AllocationMode, sub.Count, sub.Selectors, sub.Tolerations, inUse, notes))
			}
		}
	}
	for n := range notes {
		r.Notes = append(r.Notes, n)
	}
	sort.Strings(r.Notes)
	return r
}

func explainOption(ctx context.Context, c Cluster, name, className string, mode resourcev1.DeviceAllocationMode, count int64,
	selectors []resourcev1.DeviceSelector, tolerations []resourcev1.DeviceToleration, inUse map[string]string, notes map[string]bool) OptionReport {

	o := OptionReport{Name: name, Need: int(count)}
	if mode == resourcev1.DeviceAllocationModeAll {
		o.Need = -1
	} else if o.Need == 0 {
		o.Need = 1
	}
	class, ok := c.Classes[className]
	if !ok {
		o.Error = fmt.Sprintf("DeviceClass %q not found", className)
		return o
	}

	classSel, err := compile(class.Spec.Selectors)
	if err != nil {
		o.Error = "class " + err.Error()
		return o
	}
	reqSel, err := compile(selectors)
	if err != nil {
		o.Error = err.Error()
		return o
	}
	for _, s := range reqSel {
		o.Selectors = append(o.Selectors, s.Expression)
	}

	multiPool := len(c.Slices) > 1 && distinctPools(c.Slices) > 1
	for _, slice := range c.Slices {
		for _, dev := range slice.Spec.Devices {
			id := dev.Name
			if multiPool {
				id = slice.Spec.Pool.Name + "/" + dev.Name
			}
			input := dracel.Device{Driver: slice.Spec.Driver, Attributes: dev.Attributes, Capacity: dev.Capacity, AllowMultipleAllocations: dev.AllowMultipleAllocations}
			v := DeviceVerdict{Device: id, Reason: Available}

			switch {
			case !matchesAll(ctx, classSel, input):
				v.Reason = WrongClass
			case !matchesAll(ctx, reqSel, input):
				v.Reason, v.Detail = SelectorFalse, attrDetail(reqSel, dev.Attributes)
			default:
				if t := untolerated(deviceTaints(c.TaintRules, slice, dev), tolerations); t != "" {
					v.Reason, v.Detail = Tainted, t
					if t == "gpufit.io/unfit-training" && slice.Spec.NodeName != nil {
						if n := c.Nodes[*slice.Spec.NodeName]; n != nil {
							notes[fmt.Sprintf("node %s is in the %s pool (%s); training claims don't tolerate gpufit.io/unfit-training",
								n.Name, n.Labels["gpufit.io/pool"], n.Annotations["gpufit.io/pool-reason"])] = true
						}
					}
				} else if who, used := inUse[key(slice.Spec.Driver, slice.Spec.Pool.Name, dev.Name)]; used &&
					(dev.AllowMultipleAllocations == nil || !*dev.AllowMultipleAllocations) {
					v.Reason, v.Detail = InUse, who
				}
			}
			if v.Reason != WrongClass {
				o.Verdicts = append(o.Verdicts, v)
			}
		}
	}
	return o
}

// compile turns CEL selectors into programs with the scheduler's compiler.
func compile(sels []resourcev1.DeviceSelector) ([]dracel.CompilationResult, error) {
	compiler := dracel.GetCompiler(dracel.Features{})
	var out []dracel.CompilationResult
	for _, s := range sels {
		if s.CEL == nil {
			continue
		}
		res := compiler.CompileCELExpression(s.CEL.Expression, dracel.Options{})
		if res.Error != nil {
			return nil, fmt.Errorf("CEL %q: %v", s.CEL.Expression, res.Error)
		}
		out = append(out, res)
	}
	return out, nil
}

func matchesAll(ctx context.Context, sels []dracel.CompilationResult, d dracel.Device) bool {
	for _, s := range sels {
		ok, _, err := s.DeviceMatches(ctx, d)
		if err != nil || !ok { // a runtime error (e.g. missing attribute) means "no match" to the scheduler too
			return false
		}
	}
	return true
}

// attrDetail shows the values of the attributes a failing selector reads,
// e.g. `device.attributes["gpufit.io"].trainingScore >= 95` -> "trainingScore 70".
var attrRef = regexp.MustCompile(`device\.attributes\["([^"]+)"\]\.(\w+)`)

func attrDetail(sels []dracel.CompilationResult, attrs map[resourcev1.QualifiedName]resourcev1.DeviceAttribute) string {
	var parts []string
	for _, s := range sels {
		for _, m := range attrRef.FindAllStringSubmatch(s.Expression, -1) {
			a, ok := attrs[resourcev1.QualifiedName(m[1]+"/"+m[2])]
			switch {
			case !ok:
				parts = append(parts, m[2]+" missing")
			case a.IntValue != nil:
				parts = append(parts, fmt.Sprintf("%s %d", m[2], *a.IntValue))
			case a.StringValue != nil:
				parts = append(parts, fmt.Sprintf("%s %q", m[2], *a.StringValue))
			case a.BoolValue != nil:
				parts = append(parts, fmt.Sprintf("%s %t", m[2], *a.BoolValue))
			}
		}
	}
	return strings.Join(parts, ", ")
}

// deviceTaints: taints in the slice plus DeviceTaintRules that select the device.
func deviceTaints(rules []resourcev1.DeviceTaintRule, slice resourcev1.ResourceSlice, dev resourcev1.Device) []resourcev1.DeviceTaint {
	taints := append([]resourcev1.DeviceTaint(nil), dev.Taints...)
	for _, r := range rules {
		s := r.Spec.DeviceSelector
		if s != nil && ((s.Driver != nil && *s.Driver != slice.Spec.Driver) ||
			(s.Pool != nil && *s.Pool != slice.Spec.Pool.Name) ||
			(s.Device != nil && *s.Device != dev.Name)) {
			continue
		}
		taints = append(taints, r.Spec.Taint)
	}
	return taints
}

// untolerated returns the key of the first NoSchedule/NoExecute taint that no
// toleration matches, or "".
func untolerated(taints []resourcev1.DeviceTaint, tols []resourcev1.DeviceToleration) string {
	for _, t := range taints {
		if t.Effect != resourcev1.DeviceTaintEffectNoSchedule && t.Effect != resourcev1.DeviceTaintEffectNoExecute {
			continue
		}
		tolerated := false
		for _, tol := range tols {
			keyOK := tol.Key == "" || tol.Key == t.Key
			valOK := tol.Operator == resourcev1.DeviceTolerationOpExists || tol.Value == t.Value
			effOK := tol.Effect == "" || tol.Effect == t.Effect
			if keyOK && valOK && effOK {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return t.Key
		}
	}
	return ""
}

// usedBy maps driver/pool/device -> "namespace/pod" (or claim) for every
// allocated claim except the one being explained.
func usedBy(claims []resourcev1.ResourceClaim, self types.UID) map[string]string {
	out := map[string]string{}
	for _, cl := range claims {
		if cl.Status.Allocation == nil || cl.UID == self {
			continue
		}
		who := cl.Namespace + "/" + cl.Name
		if len(cl.Status.ReservedFor) > 0 {
			who = cl.Namespace + "/" + cl.Status.ReservedFor[0].Name
		}
		for _, res := range cl.Status.Allocation.Devices.Results {
			out[key(res.Driver, res.Pool, res.Device)] = who
		}
	}
	return out
}

func key(driver, pool, dev string) string { return driver + "/" + pool + "/" + dev }

func distinctPools(slices []resourcev1.ResourceSlice) int {
	seen := map[string]bool{}
	for _, s := range slices {
		seen[s.Spec.Driver+"/"+s.Spec.Pool.Name] = true
	}
	return len(seen)
}
