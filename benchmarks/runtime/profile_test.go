package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProfileOutputsAreFreshAndMutuallyExclusive(t *testing.T) {
	c := testConfig()
	c.CPUProfile = "cpu.pprof"
	c.AllocProfile = "alloc.pprof"
	if c.validate() == nil {
		t.Fatal("combined profiles accepted")
	}
	c.AllocProfile = ""
	c.CPUProfile = filepath.Join(t.TempDir(), "existing.pprof")
	if err := os.WriteFile(c.CPUProfile, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveProfile(c); err == nil {
		t.Fatal("existing profile overwritten")
	}
	b, _ := os.ReadFile(c.CPUProfile)
	if string(b) != "keep" {
		t.Fatal("profile changed")
	}
	c.Output = filepath.Join(t.TempDir(), "report.json")
	c.DSNEnv = "LIQUIDITYLAB_PROFILE_INVALID_DSN"
	t.Setenv(c.DSNEnv, "invalid-development-DSN")
	if err := execute(c); err == nil || !strings.Contains(err.Error(), "reserve fresh profiling") {
		t.Fatalf("profile reservation did not precede DB: %v", err)
	}
}

func TestSeparateProfilesFinishAndCleanup(t *testing.T) {
	for _, kind := range []string{"cpu", "allocs"} {
		t.Run(kind, func(t *testing.T) {
			c := testConfig()
			path := filepath.Join(t.TempDir(), kind+".pprof")
			if kind == "cpu" {
				c.CPUProfile = path
			} else {
				c.AllocProfile = path
			}
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
			p.StopCPU()
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
		})
	}
}
