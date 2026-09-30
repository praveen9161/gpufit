// gpufit-agent turns per-GPU health ratios into workload fitness scores.
//
// Usage:
//
//	gpufit-agent run [flags]                                     # in-cluster loop, see run.go
//	gpufit-agent explain -xml gpu.xml                            # nvidia-smi readings -> ratios -> scores
//	gpufit-agent -metrics testdata/metrics.json                  # print a table
//	gpufit-agent -metrics testdata/metrics.json -o configmap | kubectl apply -f -
//
// The ConfigMap is read by the (forked) dra-example-driver, which publishes
// the scores as gpufit.io/<workload>Score device attributes.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/praveen9161/gpufit/agent/internal/fitness"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "run" || os.Args[1] == "explain") {
		sub := runAgent
		if os.Args[1] == "explain" {
			sub = explainXML
		}
		if err := sub(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "gpufit-agent:", err)
			os.Exit(1)
		}
		return
	}

	path := flag.String("metrics", "testdata/metrics.json", "JSON file: {\"gpu-0\": {\"compute\": 1.0, ...}, ...}")
	output := flag.String("o", "table", "output format: table | configmap")
	namespace := flag.String("namespace", "dra-example-driver", "namespace of the scores ConfigMap (the driver's namespace)")
	name := flag.String("name", "gpufit-scores", "name of the scores ConfigMap")
	flag.Parse()

	var err error
	switch *output {
	case "table":
		err = run(*path)
	case "configmap":
		err = runConfigMap(*path, *namespace, *name)
	default:
		err = fmt.Errorf("unknown -o %q, want table or configmap", *output)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gpufit-agent:", err)
		os.Exit(1)
	}
}

func readMetrics(path string) (map[string]fitness.Health, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var gpus map[string]fitness.Health
	if err := json.Unmarshal(data, &gpus); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return gpus, nil
}

func run(path string) error {
	gpus, err := readMetrics(path)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(gpus))
	for n := range gpus {
		names = append(names, n)
	}
	sort.Strings(names)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprint(w, "GPU")
	for _, p := range fitness.DefaultProfiles {
		fmt.Fprintf(w, "\t%s", p.Name)
	}
	fmt.Fprintln(w, "\tWHY (training)")

	for _, n := range names {
		fmt.Fprint(w, n)
		var why string
		for i, p := range fitness.DefaultProfiles {
			r, err := fitness.Score(gpus[n], p)
			if err != nil {
				return fmt.Errorf("%s: %w", n, err)
			}
			fmt.Fprintf(w, "\t%d", r.Score)
			if i == 0 {
				why = r.Reason
			}
		}
		fmt.Fprintf(w, "\t%s\n", why)
	}
	return w.Flush()
}

// runConfigMap prints a ConfigMap whose scores.json is
// {"gpu-0": {"training": 100, "finetune": 100, "inference": 100}, ...}.
// JSON is valid input for kubectl apply, so no YAML library is needed.
func runConfigMap(path, namespace, name string) error {
	gpus, err := readMetrics(path)
	if err != nil {
		return err
	}

	results, err := fitness.ScoreAll(gpus)
	if err != nil {
		return err
	}
	scoresJSON, err := json.Marshal(fitness.ScoreValues(results)) // map keys are sorted, so output is stable
	if err != nil {
		return err
	}

	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"data":       map[string]string{"scores.json": string(scoresJSON)},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(cm)
}
