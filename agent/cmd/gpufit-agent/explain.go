package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"github.com/praveen9161/gpufit/agent/internal/collect"
	"github.com/praveen9161/gpufit/agent/internal/fitness"
)

// explainXML is "gpufit-agent explain": show, for every GPU in an
// `nvidia-smi -q -x` report, the raw readings, the ratios with the reason for
// each, and the resulting workload scores. First thing to run on a real GPU:
//
//	nvidia-smi -q -x > gpu.xml && gpufit-agent explain -xml gpu.xml
func explainXML(args []string) error {
	fs := flag.NewFlagSet("explain", flag.ExitOnError)
	path := fs.String("xml", "", "file with `nvidia-smi -q -x` output (- for stdin)")
	exponent := fs.Float64("power-exponent", collect.DefaultParams.PowerExponent, "compute ~ power_ratio^exponent")
	fs.Parse(args)
	if *path == "" {
		return fmt.Errorf("-xml is required")
	}

	var data []byte
	var err error
	if *path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(*path)
	}
	if err != nil {
		return err
	}
	readings, err := collect.ParseXML(data)
	if err != nil {
		return err
	}

	p := collect.DefaultParams
	p.PowerExponent = *exponent
	for _, r := range readings {
		e := collect.Ratios(r, p)
		fmt.Printf("%s  %s  %s\n", r.Name, r.Product, r.UUID)
		fmt.Printf("  readings: power %s/%s W, temp %s C (slowdown %s C), active events: %s\n",
			f0(r.PowerLimitW), f0(r.DefaultPowerLimitW), f0(r.TempC), f0(r.SlowdownTempC), orNone(r.ActiveEvents))
		for _, c := range fitness.Components {
			fmt.Printf("  %-8s %.2f  %s\n", c, e.Health[c], e.Why[c])
		}
		var scores []string
		for _, prof := range fitness.DefaultProfiles {
			res, err := fitness.Score(e.Health, prof)
			if err != nil {
				return err
			}
			s := fmt.Sprintf("%s %d", prof.Name, res.Score)
			if res.Reason != "healthy" {
				s += " (" + res.Reason + ")"
			}
			scores = append(scores, s)
		}
		fmt.Printf("  scores:  %s\n\n", strings.Join(scores, ", "))
	}
	return nil
}

func f0(v float64) string {
	if math.IsNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%.0f", v)
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}
