package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liquiditylab/liquiditylab/internal/historicalreplay"
	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
)

func TestReplayComparisonCLIUsesVerifiedArchiveAndExclusiveOutput(t *testing.T) {
	root := t.TempDir()
	const fixture = "../../examples/recordings/kraken-btcusd-20261028/"
	m, err := marketarchive.Import(context.Background(), marketarchive.Config{RawPath: fixture + "public-capture.jsonl", ManifestPath: fixture + "public-capture.manifest.json", ArchiveRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "comparison.json")
	args := []string{"replay", "compare", "--archive", root, "--id", m.ID, "--spec", "../../examples/replay/retained-short-compare.json", "--output", output}
	var stdout, stderr bytes.Buffer
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var report historicalreplay.Report
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	if !report.EstimateOnly || report.SourceSessionBinding != nil || report.Archive.ID != m.ID || len(report.Alternatives) != 1 || report.Selection.RequiredEndNS != 1100000000 || !report.Reference.Comparable || !report.Alternatives[0].Comparable {
		t.Fatalf("wrong comparison: %+v", report)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("report is not private")
	}
	stdout.Reset()
	stderr.Reset()
	if err := run(args, &stdout, &stderr); err == nil || stdout.Len() != 0 {
		t.Fatal("existing report overwritten or reported success")
	}
	again, _ := os.ReadFile(output)
	if !bytes.Equal(again, b) {
		t.Fatal("failed repeated command changed original report")
	}
	if files, _ := filepath.Glob(filepath.Join(filepath.Dir(output), ".replay-report-*")); len(files) != 0 {
		t.Fatal("staging files leaked")
	}
}

func TestReplayArgumentsAndHelp(t *testing.T) {
	for _, args := range [][]string{{"replay"}, {"replay", "unknown"}, {"replay", "compare"}, {"replay", "compare", "positional"}, {"replay", "compare", "--archive", "/tmp", "--id", "../invalid", "--spec", "missing", "--output", "missing"}} {
		var stdout, stderr bytes.Buffer
		if err := run(args, &stdout, &stderr); err == nil || stdout.Len() != 0 {
			t.Fatalf("invalid args succeeded: %v", args)
		}
	}
	for _, args := range [][]string{{"help"}, {"replay", "help"}} {
		var stdout, stderr bytes.Buffer
		if err := run(args, &stdout, &stderr); err != nil || !strings.Contains(stdout.String(), "replay") {
			t.Fatal("replay help missing")
		}
	}
}

type cancelReplayWrite struct {
	context.Context
	remaining int
}

func (c *cancelReplayWrite) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestReplayReportPublicationIsCompleteExclusiveAndCancellable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "report.json")
	data := bytes.Repeat([]byte("x"), 130000)
	ctx := &cancelReplayWrite{Context: context.Background(), remaining: 3}
	if err := writeReplayReport(ctx, target, data); !errors.Is(err, context.Canceled) {
		t.Fatalf("staging cancellation ignored: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("cancelled staging published an artifact")
	}
	if files, _ := filepath.Glob(filepath.Join(dir, ".replay-report-*")); len(files) != 0 {
		t.Fatal("cancelled staging leaked")
	}
	if err := writeReplayReport(context.Background(), target, data); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("published incomplete artifact")
	}
	if err := writeReplayReport(context.Background(), target, []byte("different")); err == nil {
		t.Fatal("overwrote existing artifact")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeReplayReport(context.Background(), link, []byte("different")); err == nil {
		t.Fatal("overwrote symlink")
	}
	got, _ = os.ReadFile(target)
	if !bytes.Equal(got, data) {
		t.Fatal("exclusive failure changed existing artifact")
	}
	if err := writeReplayReport(context.Background(), filepath.Join(dir, "large"), make([]byte, historicalreplay.MaxReportBytes+1)); err == nil {
		t.Fatal("oversized report accepted")
	}
}
