package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

type paperMetricsContextKey struct{}

type paperMetricsReport struct {
	Version      string              `json:"version"`
	CaptureState string              `json:"capture_state"`
	CapturedAt   time.Time           `json:"captured_at"`
	KnownFinal   bool                `json:"financial_state_known_final"`
	Metrics      *telemetry.Snapshot `json:"metrics,omitempty"`
	Pool         *pg.PoolStatistics  `json:"pool,omitempty"`
}

// Reserve the operator's new local output before taking account ownership. A
// killed process leaves an explicit incomplete marker, never a capacity report.
func openPaperMetrics(path string) (*telemetry.Recorder, *os.File, error) {
	if path == "" {
		return nil, nil, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, nil, errors.New("paper metrics require a new writable local output path")
	}
	initial := paperMetricsReport{Version: "paper-metrics-v1", CaptureState: "incomplete", CapturedAt: time.Now().UTC()}
	if err := json.NewEncoder(f).Encode(initial); err != nil {
		_ = f.Close()
		return nil, nil, errors.New("paper metrics initial marker could not be written")
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, nil, errors.New("paper metrics initial marker could not be synced")
	}
	return telemetry.New(), f, nil
}

func (p *paperDaemon) finishMetrics(knownFinal bool) (resultErr error) {
	if p.metricsOutput == nil {
		return nil
	}
	f := p.metricsOutput
	defer func() {
		if err := f.Close(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("paper metrics output could not be closed"))
		}
	}()
	metrics, pool := p.metrics.Snapshot(), p.store.PoolStats()
	report := paperMetricsReport{Version: "paper-metrics-v1", CaptureState: "shutdown_snapshot", CapturedAt: time.Now().UTC(), KnownFinal: knownFinal, Metrics: &metrics, Pool: &pool}
	encoded, err := json.Marshal(report)
	if err != nil {
		return errors.New("paper metrics snapshot could not be encoded")
	}
	// Append one newline-terminated frame. A partial append leaves the synced
	// initial marker intact; readers ignore an incomplete trailing JSONL frame.
	_, err = f.Write(append(encoded, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		return errors.New("paper metrics shutdown snapshot could not be written")
	}
	return nil
}

func validatePaperOutputPaths(opt paperServerOptions) error {
	if opt.metricsOutput == "" || opt.source != "kraken" || opt.captureOutput == "" {
		return nil
	}
	metrics, err := prospectiveOutputPath(opt.metricsOutput)
	if err != nil {
		return errors.New("paper metrics output path cannot be resolved")
	}
	for _, path := range []string{opt.captureOutput, opt.captureOutput + ".manifest.json", opt.captureOutput + ".manifest.json.tmp"} {
		capture, err := prospectiveOutputPath(path)
		if err != nil {
			return errors.New("paper capture output path cannot be resolved")
		}
		if metrics == capture {
			return errors.New("paper metrics output must differ from capture and manifest paths")
		}
	}
	return nil
}

// Resolve existing directory aliases while allowing capture's not-yet-created
// directories. This comparison never creates files or takes account ownership.
func prospectiveOutputPath(path string) (string, error) {
	// Abs/Dir clean '..' before following symlinks, unlike filesystem lookup.
	// Preserve components until EvalSymlinks resolves the existing ancestor.
	abs := path
	if !filepath.IsAbs(abs) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		abs = cwd + string(filepath.Separator) + path
	}
	parent, suffix := filepath.Split(abs)
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		trimmed := strings.TrimRight(parent, string(filepath.Separator))
		next, part := filepath.Split(trimmed)
		if !errors.Is(err, os.ErrNotExist) || part == "" || next == parent {
			return "", err
		}
		suffix = part + string(filepath.Separator) + suffix
		parent = next
	}
}
