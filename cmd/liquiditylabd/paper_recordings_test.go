//go:build linux

package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/liquiditylab/liquiditylab/internal/marketdata"
)

func TestPaperRecordingAdmissionBoundsAndPreservation(t *testing.T) {
	const raw = int64(1024)
	reserve := raw + 2*marketdata.MaxManifestBytes
	for _, extra := range []int64{-1, 0, 1} {
		t.Run(map[int64]string{-1: "below", 0: "exact", 1: "above"}[extra], func(t *testing.T) {
			root := t.TempDir()
			old := filepath.Join(root, "orphan.manifest.json.tmp")
			if err := os.WriteFile(old, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			a, err := admitPaperRecording(root, reserve+9+extra, raw)
			if (err == nil) != (extra >= 0) {
				t.Fatalf("admission=%v", err)
			}
			if a != nil {
				defer a.Close()
			}
			got, err := os.ReadFile(old)
			if err != nil || string(got) != "unchanged" {
				t.Fatalf("existing file changed: %q %v", got, err)
			}
			entries, _ := os.ReadDir(root)
			if extra < 0 && len(entries) != 1 {
				t.Fatal("refusal created files")
			}
		})
	}
}

func TestPaperRecordingAdmissionRejectsUnsafeEntriesAndReleasesLock(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "unsafe")
			var err error
			if kind == "symlink" {
				err = os.Symlink(t.TempDir(), path)
			} else {
				err = syscall.Mkfifo(path, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if a, err := admitPaperRecording(root, 1<<20, 1024); err == nil {
				a.Close()
				t.Fatal("unsafe entry admitted")
			}
			// A failed scan must release its lock. Test by separately locking the directory.
			f, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := lockPaperRecordingDirectory(f); err != nil {
				t.Fatalf("lock leaked: %v", err)
			}
		})
	}
}

func TestPaperRecordingAdmissionProcessContender(t *testing.T) {
	if root := os.Getenv("LIQUIDITYLAB_RECORDING_LOCK_TEST"); root != "" {
		a, err := admitPaperRecording(root, 1<<20, 1024)
		if err == nil {
			a.Close()
			t.Fatal("second process admitted")
		}
		if !strings.Contains(err.Error(), "lock unavailable") {
			t.Fatal(err)
		}
		return
	}
	root := t.TempDir()
	a, err := admitPaperRecording(root, 1<<20, 1024)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPaperRecordingAdmissionProcessContender$")
	cmd.Env = append(os.Environ(), "LIQUIDITYLAB_RECORDING_LOCK_TEST="+root)
	if output, err := cmd.CombinedOutput(); err != nil {
		a.Close()
		t.Fatalf("contender: %v %s", err, output)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := admitPaperRecording(root, 1<<20, 1024)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	b.Close()
}

func TestPaperRecordingAdmissionEntryBound(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < maxRecordingEntries; i++ {
		if err := os.Mkdir(filepath.Join(root, strconv.Itoa(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if a, err := admitPaperRecording(root, 1<<20, 1024); err == nil {
		a.Close()
		t.Fatal("entry flood admitted")
	}
}

func TestPaperRecordingOptionsAndPreDatabaseRefusal(t *testing.T) {
	opt := daemonOptions(t)
	opt.source = "kraken"
	opt.recordingRoot = t.TempDir()
	opt.maxRecordingBytes = 1
	err := servePaper(context.Background(), opt, "unreachable invalid database", daemonTestToken, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "recording storage admission refused") {
		t.Fatalf("did not refuse before DB: %v", err)
	}
	opt.captureOutput = filepath.Join(t.TempDir(), "raw.jsonl")
	if err := opt.validate(); err == nil {
		t.Fatal("both output modes admitted")
	}
	opt.captureOutput = ""
	opt.recordingRoot = "relative"
	if err := opt.validate(); err == nil {
		t.Fatal("relative root admitted")
	}
	opt.recordingRoot = t.TempDir()
	opt.source = "synthetic"
	if err := opt.validate(); err == nil {
		t.Fatal("synthetic root admitted")
	}
}

func TestPaperRecordingMetricsAliasesAndRootSymlink(t *testing.T) {
	opt := daemonOptions(t)
	opt.source = "kraken"
	opt.recordingRoot = t.TempDir()
	opt.maxRecordingBytes = 1 << 30
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(opt.recordingRoot, alias); err != nil {
		t.Fatal(err)
	}
	for _, metrics := range []string{filepath.Join(opt.recordingRoot, "metrics.jsonl"), filepath.Join(alias, "new", "metrics.jsonl")} {
		opt.metricsOutput = metrics
		if err := opt.validate(); err == nil {
			t.Fatal("metrics alias inside root admitted")
		}
	}
	for _, path := range []string{alias, alias + "/"} {
		if a, err := admitPaperRecording(path, 1<<20, 1024); err == nil {
			a.Close()
			t.Fatal("symlink root admitted")
		}
	}
}

func TestPaperRecordingAdmissionReleasedAfterStaticFailure(t *testing.T) {
	opt := daemonOptions(t)
	opt.source = "kraken"
	opt.recordingRoot = t.TempDir()
	opt.maxRecordingBytes = 1 << 30
	opt.webDir = filepath.Join(t.TempDir(), "missing")
	if err := servePaper(context.Background(), opt, "unreachable invalid database", daemonTestToken, io.Discard); err == nil {
		t.Fatal("missing static assets accepted")
	}
	a, err := admitPaperRecording(opt.recordingRoot, 1<<30, 1024)
	if err != nil {
		t.Fatalf("startup leaked lock: %v", err)
	}
	a.Close()
}
