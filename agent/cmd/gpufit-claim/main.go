// gpufit-claim prints a ResourceClaimTemplate that asks for GPUs by GPUFit
// score, trying the best tier first.
//
// Usage:
//
//	gpufit-claim -workload training -min 80 -count 2 | kubectl apply -f -
//
// Then reference it from a pod:
//
//	resourceClaims:
//	- name: gpus
//	  resourceClaimTemplateName: training-min80-2gpu
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/praveen9161/gpufit/agent/internal/claims"
)

func main() {
	o := claims.Options{}
	flag.StringVar(&o.Workload, "workload", "training", "workload type: training | finetune | inference")
	flag.IntVar(&o.Min, "min", 80, "lowest acceptable score (0-100)")
	flag.IntVar(&o.Count, "count", 1, "number of GPUs")
	flag.StringVar(&o.Name, "name", "", "template name (default <workload>-min<min>-<count>gpu)")
	flag.StringVar(&o.Namespace, "namespace", "gpufit-lab", "namespace of the template")
	flag.StringVar(&o.DeviceClass, "device-class", "gpu.example.com", "DRA DeviceClass of the GPUs")
	prefer := flag.String("prefer", "95,90", "comma-separated higher scores to try before -min")
	flag.Parse()

	var err error
	if o.Prefer, err = parseInts(*prefer); err == nil {
		err = claims.Write(os.Stdout, o)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gpufit-claim:", err)
		os.Exit(1)
	}
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("-prefer: %q is not a number", f)
		}
		out = append(out, n)
	}
	return out, nil
}
