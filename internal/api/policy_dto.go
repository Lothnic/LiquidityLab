package api

import (
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
)

// PolicyRuntimeDTO exposes proposal provenance without observations, owner
// identities, transport configuration or worker errors.
type PolicyRuntimeDTO struct {
	PolicySHA256          string    `json:"policy_sha256"`
	Profile               string    `json:"profile"`
	Fallback              string    `json:"fallback"`
	PromotionStatus       string    `json:"promotion_status"`
	Result                string    `json:"result"`
	Reason                string    `json:"reason"`
	Action                d.Action  `json:"action"`
	ProposedAction        *d.Action `json:"proposed_action,omitempty"`
	ObservationSnapshotID string    `json:"observation_snapshot_id"`
	AppliedSnapshotID     string    `json:"applied_snapshot_id"`
	ExpiresAt             time.Time `json:"expires_at"`
	ResolvedAt            time.Time `json:"resolved_at"`
}

// PolicyConfigDTO exposes the account default for the next parent. An existing
// parent retains its own immutable bundle, exposed through PolicyRuntimeDTO.
type PolicyConfigDTO struct {
	PolicySHA256    string `json:"policy_sha256"`
	Profile         string `json:"profile"`
	Fallback        string `json:"fallback"`
	PromotionStatus string `json:"promotion_status"`
}

func policyConfigProjection(s rt.State) *PolicyConfigDTO {
	if s.PolicyBundle == nil {
		return nil
	}
	b := s.PolicyBundle
	return &PolicyConfigDTO{PolicySHA256: b.PolicySHA256, Profile: b.Profile, Fallback: b.Fallback, PromotionStatus: b.PromotionStatus}
}

func policyProjection(s rt.State) *PolicyRuntimeDTO {
	if s.Parent == nil || s.Parent.PolicyBundle == nil {
		return nil
	}
	b := s.Parent.PolicyBundle
	out := &PolicyRuntimeDTO{PolicySHA256: b.PolicySHA256, Profile: b.Profile, Fallback: b.Fallback, PromotionStatus: b.PromotionStatus, Result: "waiting", Action: d.Hold}
	audit := s.LastPolicyDecision
	if s.PolicyPending != nil && s.PolicyPending.ParentID == s.Parent.ID {
		audit = s.PolicyPending
	}
	if audit != nil && audit.ParentID == s.Parent.ID {
		out.Result = audit.Result
		out.Reason = audit.Reason
		out.Action = audit.Action
		out.ProposedAction = audit.ProposedAction
		out.ObservationSnapshotID = audit.MarketSnapshotID
		out.AppliedSnapshotID = audit.AppliedMarketSnapshotID
		out.ExpiresAt = audit.ExpiresAt
		out.ResolvedAt = audit.ResolvedAt
	}
	return out
}
