package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sync"
	"time"
)

const maxProfileBytes int64 = 32 << 20

type profileReport struct {
	Kind        string    `json:"kind"`
	Name        string    `json:"artifact_name,omitempty"`
	Status      string    `json:"status"`
	Boundary    string    `json:"boundary"`
	Started     time.Time `json:"started_utc,omitempty"`
	Finished    time.Time `json:"finished_utc,omitempty"`
	SHA256      string    `json:"sha256,omitempty"`
	Bytes       int64     `json:"bytes,omitempty"`
	BlockRateNS int       `json:"block_profile_rate_ns,omitempty"`
}
type profileOutput struct {
	file     *os.File
	mu       sync.Mutex
	bytes    int64
	writeErr error
	active   bool
	finished bool
	report   profileReport
}

func reserveProfile(c config) (*profileOutput, error) {
	p := &profileOutput{report: profileReport{Kind: "none", Status: "disabled", Boundary: "no profiler"}}
	path := c.CPUProfile
	if path != "" {
		p.report.Kind = "cpu"
		p.report.Boundary = "CPU samples from immediately before measurement schedule construction through measurement and accepted-call drain; separate profiling run"
	} else if c.AllocProfile != "" {
		path = c.AllocProfile
		p.report.Kind = "allocs"
		p.report.Boundary = "Cumulative sampled allocation stacks/counts from process startup, including setup and warmup; captured after measured resource endpoints; separate profiling run; no object contents"
	} else if c.BlockProfile != "" {
		path = c.BlockProfile
		p.report.Kind = "block"
		p.report.BlockRateNS = 1
		p.report.Boundary = "Go block sampling enabled at rate 1 immediately before measured schedule construction through measurement and accepted-call drain; disabled before resource endpoints; cumulative completed Go channel/synchronization blocking stacks/counts written after endpoints; no reset or strict interval delta; excludes PostgreSQL fsync and external worker blocking; separate profiling run; no object contents"
	}
	if path == "" {
		return p, nil
	}
	f, err := freshFile(path)
	if err != nil {
		return nil, errors.New("reserve fresh profiling output failed")
	}
	p.file = f
	p.report.Name = filepath.Base(path)
	p.report.Status = "reserved"
	return p, nil
}
func (p *profileOutput) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.writeErr != nil {
		return 0, p.writeErr
	}
	if p.bytes+int64(len(b)) > maxProfileBytes {
		p.writeErr = errors.New("profile exceeded 32 MiB bound")
		return 0, p.writeErr
	}
	n, err := p.file.Write(b)
	p.bytes += int64(n)
	p.writeErr = err
	return n, err
}
func (p *profileOutput) StartMeasurement() error {
	if p.file == nil {
		return nil
	}
	if p.report.Kind == "cpu" || p.report.Kind == "block" {
		p.report.Started = time.Now().UTC()
	}
	if p.report.Kind == "cpu" {
		if err := pprof.StartCPUProfile(p); err != nil {
			p.report.Status = "start_failed"
			return errors.New("start CPU profiler failed")
		}
		p.active = true
		p.report.Status = "running"
	}
	if p.report.Kind == "block" {
		// This standalone benchmark owns the process-wide setting. Rate 1
		// records every completed Go blocking event while enabled; it does not
		// reset the runtime's cumulative blocking history.
		runtime.SetBlockProfileRate(p.report.BlockRateNS)
		p.active = true
		p.report.Status = "running"
	}
	return nil
}
func (p *profileOutput) FinishMeasurement() error {
	if p.file == nil || p.finished {
		return nil
	}
	p.StopSampling()
	if p.report.Kind == "allocs" {
		p.report.Started = time.Now().UTC()
		if err := pprof.Lookup("allocs").WriteTo(p, 0); err != nil {
			p.report.Status = "write_failed"
			return errors.New("write cumulative allocation profile failed")
		}
	}
	if p.report.Kind == "block" {
		if err := pprof.Lookup("block").WriteTo(p, 0); err != nil {
			p.report.Status = "write_failed"
			return errors.New("write cumulative blocking profile failed")
		}
	}
	p.finished = true
	if p.report.Finished.IsZero() {
		p.report.Finished = time.Now().UTC()
	}
	if p.writeErr != nil {
		p.report.Status = "write_failed"
		return errors.New("profiling output write failed")
	}
	if err := p.file.Sync(); err != nil {
		p.report.Status = "sync_failed"
		return errors.New("profiling output sync failed")
	}
	sum, n, err := fileSHA256(p.file.Name())
	if err != nil {
		p.report.Status = "hash_failed"
		return errors.New("profiling checksum failed")
	}
	p.report.SHA256 = sum
	p.report.Bytes = n
	p.report.Status = "complete"
	return nil
}
func (p *profileOutput) Close() {
	if p == nil {
		return
	}
	p.StopSampling()
	if p.file != nil {
		_ = p.file.Close()
	}
	if p.report.Status == "reserved" || p.report.Status == "running" {
		p.report.Status = "incomplete"
	}
}

func (p *profileOutput) StopSampling() {
	if p.active {
		if p.report.Kind == "block" {
			runtime.SetBlockProfileRate(0)
		} else {
			pprof.StopCPUProfile()
		}
		p.active = false
		p.report.Finished = time.Now().UTC()
	}
}
