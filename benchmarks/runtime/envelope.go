package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"strings"
	"time"

	"github.com/liquiditylab/liquiditylab/internal/inference"
)

type envelopeConfig struct {
	ReadMode            string        `json:"read_mode"`
	HTTPStatePercent    int           `json:"http_state_percent"`
	HTTPSessionsPercent int           `json:"http_sessions_percent"`
	HTTPReportPercent   int           `json:"http_report_percent"`
	HTTPTimeout         time.Duration `json:"http_timeout_ns"`
	PolicyTarget        string        `json:"-"`
	PolicySHA256        string        `json:"policy_sha256,omitempty"`
	MockPolicyDelay     time.Duration `json:"mock_policy_delay_ns"`
	PolicyTimeout       time.Duration `json:"policy_timeout_ns"`
	PolicyFallback      string        `json:"policy_fallback"`
	PolicyProfile       string        `json:"policy_profile"`
	CPUProfile          string        `json:"-"`
	AllocProfile        string        `json:"-"`
	BlockProfile        string        `json:"-"`
}

func defaultEnvelope() envelopeConfig {
	return envelopeConfig{ReadMode: "actor", HTTPStatePercent: 60, HTTPSessionsPercent: 20, HTTPReportPercent: 20, HTTPTimeout: time.Second, MockPolicyDelay: -1, PolicyTimeout: 100 * time.Millisecond, PolicyFallback: "baseline", PolicyProfile: "paper"}
}
func envelopeFlags(c *config) {
	flag.StringVar(&c.ReadMode, "read-mode", c.ReadMode, "actor views or HTTP polling: actor|http")
	flag.IntVar(&c.HTTPStatePercent, "http-state-percent", c.HTTPStatePercent, "HTTP account state read percentage")
	flag.IntVar(&c.HTTPSessionsPercent, "http-sessions-percent", c.HTTPSessionsPercent, "HTTP session-list read percentage")
	flag.IntVar(&c.HTTPReportPercent, "http-report-percent", c.HTTPReportPercent, "HTTP session-report read percentage")
	flag.DurationVar(&c.HTTPTimeout, "http-timeout", c.HTTPTimeout, "HTTP client/API timeout (10ms..2s)")
	flag.StringVar(&c.PolicyTarget, "policy-target", "", "explicit local real policy worker target; omitted from report")
	flag.StringVar(&c.PolicySHA256, "policy-sha256", "", "required real worker bundle SHA256")
	flag.DurationVar(&c.MockPolicyDelay, "mock-policy-delay", -1, "enable local controlled unvalidated HOLD worker with delay (0..5s); mutually exclusive with real target")
	flag.DurationVar(&c.PolicyTimeout, "policy-timeout", c.PolicyTimeout, "pinned policy deadline (1..200ms)")
	flag.StringVar(&c.PolicyFallback, "policy-fallback", c.PolicyFallback, "pinned fallback: baseline|pause")
	flag.StringVar(&c.PolicyProfile, "policy-profile", c.PolicyProfile, "unvalidated policy profile: paper|shadow")
	flag.StringVar(&c.CPUProfile, "cpu-profile", "", "new CPU profile output; measurement+drain only; separate run")
	flag.StringVar(&c.AllocProfile, "alloc-profile", "", "new cumulative allocs profile output; separate run")
	flag.StringVar(&c.BlockProfile, "block-profile", "", "new cumulative Go block profile output; rate 1 during measurement+drain; separate run")
}
func (c config) validateEnvelope() error {
	if c.ReadMode != "actor" && c.ReadMode != "http" {
		return errors.New("read mode must be actor or http")
	}
	if c.HTTPTimeout < 10*time.Millisecond || c.HTTPTimeout > 2*time.Second {
		return errors.New("HTTP timeout outside 10ms..2s")
	}
	for _, n := range []int{c.HTTPStatePercent, c.HTTPSessionsPercent, c.HTTPReportPercent} {
		if n < 0 || n > 100 {
			return errors.New("HTTP mix percentages must be within 0..100")
		}
	}
	if c.HTTPStatePercent+c.HTTPSessionsPercent+c.HTTPReportPercent != 100 {
		return errors.New("HTTP mix percentages must sum to 100")
	}
	if c.MockPolicyDelay < -1 || c.MockPolicyDelay > 5*time.Second {
		return errors.New("mock policy delay must be explicitly 0..5s")
	}
	if c.PolicyTimeout < time.Millisecond || c.PolicyTimeout > inference.MaxValidity {
		return errors.New("policy timeout outside 1..200ms")
	}
	if c.PolicyFallback != "baseline" && c.PolicyFallback != "pause" || c.PolicyProfile != "paper" && c.PolicyProfile != "shadow" {
		return errors.New("policy requires baseline|pause fallback and paper|shadow profile")
	}
	if c.PolicyTarget != "" && c.MockPolicyDelay >= 0 {
		return errors.New("real and mock policy selectors are mutually exclusive")
	}
	if c.MockPolicyDelay >= 0 && c.PolicySHA256 != "" {
		return errors.New("controlled worker pins its own synthetic SHA256")
	}
	if c.PolicyTarget != "" {
		hash, e := hex.DecodeString(c.PolicySHA256)
		if e != nil || len(hash) != 32 || c.PolicySHA256 != strings.ToLower(c.PolicySHA256) {
			return errors.New("real policy target requires lowercase SHA256")
		}
	} else if c.MockPolicyDelay < 0 && c.PolicySHA256 != "" {
		return errors.New("policy SHA256 requires a real worker target")
	}
	profiles := 0
	for _, path := range []string{c.CPUProfile, c.AllocProfile, c.BlockProfile} {
		if path != "" {
			profiles++
		}
	}
	if profiles > 1 {
		return errors.New("CPU, allocation, and blocking profiles require separate runs")
	}
	return nil
}

func (c config) readRoute(index int) string {
	if c.ReadMode != "http" {
		return ""
	}
	// Coprime stride spreads short runs across the declared 100-slot mix.
	slot := (index*37 + 17) % 100
	if slot < c.HTTPStatePercent {
		return "state"
	}
	if slot < c.HTTPStatePercent+c.HTTPSessionsPercent {
		return "sessions"
	}
	return "report"
}
