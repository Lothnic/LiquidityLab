// Native replay workload: no RPC, Python client, or model inference.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/replay"
)

func main() {
	episodes := flag.Int("episodes", 1000, "bounded number of thirty-step HOLD episodes")
	profile := flag.String("cpu-profile", "", "optional CPU profile output")
	flag.Parse()
	if *episodes < 1 || *episodes > 100000 {
		fmt.Fprintln(os.Stderr, "episodes must be in [1,100000]")
		os.Exit(2)
	}
	tape, err := replay.NewTape(replay.SyntheticEvents())
	if err != nil {
		panic(err)
	}
	engine, err := replay.NewWithTape(tape, d.DefaultPlan(), d.DefaultRules(), d.DefaultFees(), d.DefaultConfig())
	if err != nil {
		panic(err)
	}
	// Warm the exact path before measuring and beginning the profile.
	for i := 0; i < 30; i++ {
		if _, err := engine.Step(d.Hold); err != nil {
			panic(err)
		}
	}
	if *profile != "" {
		file, err := os.Create(*profile)
		if err != nil {
			panic(err)
		}
		defer file.Close()
		if err := pprof.StartCPUProfile(file); err != nil {
			panic(err)
		}
		defer pprof.StopCPUProfile()
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	steps := 0
	for episode := 0; episode < *episodes; episode++ {
		if _, err := engine.Reset(); err != nil {
			panic(err)
		}
		for !engine.Terminated && !engine.Truncated {
			if _, err := engine.Step(d.Hold); err != nil {
				panic(err)
			}
			steps++
		}
		if engine.Truncated {
			panic("invalid benchmark episode")
		}
	}
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	json.NewEncoder(os.Stdout).Encode(map[string]any{
		"scope":      "native Go HOLD replay, including resets and terminal report construction; no RPC",
		"go_version": runtime.Version(), "logical_cpus": runtime.NumCPU(),
		"episodes": *episodes, "transitions": steps, "wall_seconds": elapsed.Seconds(),
		"transitions_per_second":         float64(steps) / elapsed.Seconds(),
		"allocations_per_transition":     float64(after.Mallocs-before.Mallocs) / float64(steps),
		"allocated_bytes_per_transition": float64(after.TotalAlloc-before.TotalAlloc) / float64(steps),
	})
}
