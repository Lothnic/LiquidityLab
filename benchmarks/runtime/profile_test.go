package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestProfileOutputsAreFreshAndMutuallyExclusive(t *testing.T) {
	for _, kinds := range [][]string{{"cpu", "allocs"}, {"cpu", "block"}, {"allocs", "block"}, {"cpu", "allocs", "block"}} {
		c := testConfig()
		for _, kind := range kinds {
			setProfilePath(&c, kind, kind+".pprof")
		}
		if c.validate() == nil {
			t.Fatalf("combined profiles accepted: %v", kinds)
		}
	}
	for _, kind := range []string{"cpu", "allocs", "block"} {
		t.Run(kind, func(t *testing.T) {
			c := testConfig()
			path := filepath.Join(t.TempDir(), "existing.pprof")
			setProfilePath(&c, kind, path)
			if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := reserveProfile(c); err == nil {
				t.Fatal("existing profile overwritten")
			}
			b, _ := os.ReadFile(path)
			if string(b) != "keep" {
				t.Fatal("profile changed")
			}
			c.Output = filepath.Join(t.TempDir(), "report.json")
			c.DSNEnv = "LIQUIDITYLAB_PROFILE_INVALID_DSN"
			t.Setenv(c.DSNEnv, "invalid-development-DSN")
			if err := execute(c); err == nil || !strings.Contains(err.Error(), "reserve fresh profiling") {
				t.Fatalf("profile reservation did not precede DB: %v", err)
			}
		})
	}
}

func TestSeparateProfilesFinishAndCleanup(t *testing.T) {
	for _, kind := range []string{"cpu", "allocs", "block"} {
		t.Run(kind, func(t *testing.T) {
			c := testConfig()
			path := filepath.Join(t.TempDir(), kind+".pprof")
			setProfilePath(&c, kind, path)
			p, err := reserveProfile(c)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if err = p.StartMeasurement(); err != nil {
				t.Fatal(err)
			}
			if kind == "cpu" {
				end := time.Now().Add(15 * time.Millisecond)
				for time.Now().Before(end) {
					_ = time.Now().UnixNano()
				}
			}
			if kind == "block" {
				profileTestChannelWait()
			}
			p.StopSampling()
			if kind == "block" {
				assertBlockSamplingDisabled(t)
			}
			if err = p.FinishMeasurement(); err != nil {
				t.Fatal(err)
			}
			if p.report.Status != "complete" || p.report.SHA256 == "" || p.report.Bytes == 0 || p.active {
				t.Fatalf("profile incomplete: %+v", p.report)
			}
			b, err := os.ReadFile(path)
			if err != nil || len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
				t.Fatalf("profile is not gzip pprof: err=%v", err)
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0600 {
				t.Fatal("profile permissions not private")
			}
			assertProfileParses(t, path, kind)
		})
	}
}

func setProfilePath(c *config, kind, path string) {
	switch kind {
	case "cpu":
		c.CPUProfile = path
	case "allocs":
		c.AllocProfile = path
	case "block":
		c.BlockProfile = path
	}
}

func assertProfileParses(t *testing.T, path, kind string) {
	t.Helper()
	args := []string{"tool", "pprof", "-top"}
	if kind == "block" {
		args = append(args, "-sample_index=delay")
	}
	out, err := exec.Command("go", append(args, path)...).CombinedOutput()
	if err != nil {
		t.Fatalf("parse %s profile: %v: %s", kind, err, out)
	}
	if kind == "block" && (!strings.Contains(string(out), "Type: delay") || strings.Contains(string(out), "Total samples = 0")) {
		t.Fatalf("blocking profile has no delay samples: %s", out)
	}
}

//go:noinline
func profileTestChannelWait() {
	done := make(chan struct{})
	go func() {
		time.Sleep(5 * time.Millisecond)
		close(done)
	}()
	<-done
}

func profileTestBlockCount() int64 {
	n, _ := runtime.BlockProfile(nil)
	records := make([]runtime.BlockProfileRecord, n+16)
	n, ok := runtime.BlockProfile(records)
	if !ok {
		return -1
	}
	var count int64
	for _, record := range records[:n] {
		frames := runtime.CallersFrames(record.Stack())
		for {
			frame, more := frames.Next()
			if strings.HasSuffix(frame.Function, ".profileTestChannelWait") {
				count += record.Count
				break
			}
			if !more {
				break
			}
		}
	}
	return count
}

func assertBlockSamplingDisabled(t *testing.T) {
	t.Helper()
	before := profileTestBlockCount()
	profileTestChannelWait()
	if after := profileTestBlockCount(); before < 0 || after != before {
		t.Fatalf("blocking samples changed after sampler cleanup: before=%d after=%d", before, after)
	}
}

func TestBlockProfileFailureAndEarlyCloseDisableSampling(t *testing.T) {
	for _, mode := range []string{"close", "bound", "write"} {
		t.Run(mode, func(t *testing.T) {
			c := testConfig()
			c.BlockProfile = filepath.Join(t.TempDir(), "block.pprof")
			p, err := reserveProfile(c)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if err := p.StartMeasurement(); err != nil {
				t.Fatal(err)
			}
			profileTestChannelWait()
			switch mode {
			case "close":
				p.Close()
				if p.report.Status != "incomplete" {
					t.Fatalf("early close status: %s", p.report.Status)
				}
			case "bound":
				p.bytes = maxProfileBytes
				if _, err := p.Write([]byte{1}); err == nil {
					t.Fatal("profile exceeded byte bound")
				}
				if err := p.FinishMeasurement(); err == nil || p.report.Status != "write_failed" {
					t.Fatalf("bound failure: %v %+v", err, p.report)
				}
			case "write":
				_ = p.file.Close()
				if err := p.FinishMeasurement(); err == nil || p.report.Status != "write_failed" {
					t.Fatalf("write failure: %v %+v", err, p.report)
				}
			}
			if p.active {
				t.Fatal("blocking sampler left active")
			}
			assertBlockSamplingDisabled(t)
		})
	}
}
