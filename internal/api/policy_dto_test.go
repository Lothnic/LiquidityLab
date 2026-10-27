package api

import (
	"encoding/json"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	"strings"
	"testing"
)

func TestPolicyProjectionIsScopedAndRedactsPrivateFeatures(t *testing.T) {
	s := rt.State{Parent: &rt.Parent{ID: "current", PolicyBundle: &rt.PolicyBundle{PolicySHA256: strings.Repeat("a", 64), Profile: "paper", Fallback: "pause"}}, LastPolicyDecision: &rt.PolicyDecision{ParentID: "current", Result: "policy_action", Reason: "accepted", Action: d.PassiveNormal, OwnerGeneration: 99, Observation: []float32{123}}}
	out := policyProjection(s)
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if out.Result != "policy_action" || out.Action != d.PassiveNormal || !strings.Contains(string(body), strings.Repeat("a", 64)) {
		t.Fatalf("projection=%s", body)
	}
	for _, private := range []string{"observation\"", "owner_generation", "123"} {
		if strings.Contains(string(body), private) {
			t.Fatalf("private projection=%s", body)
		}
	}
	s.PolicyPending = &rt.PolicyDecision{ParentID: "current", Result: "pending", Action: d.Hold}
	if policyProjection(s).Result != "pending" {
		t.Fatal("pending request must override last outcome")
	}
	s.PolicyPending.ParentID = "old"
	s.LastPolicyDecision.ParentID = "old"
	if policyProjection(s).Result != "waiting" {
		t.Fatal("old parent decision leaked into new parent")
	}
	s.Parent.PolicyBundle = nil
	if policyProjection(s) != nil {
		t.Fatal("baseline parent mislabelled as a model")
	}
}
