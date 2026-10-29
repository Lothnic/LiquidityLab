package runtime

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"time"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	"github.com/liquiditylab/liquiditylab/internal/faultpoint"
	"github.com/liquiditylab/liquiditylab/internal/inference"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
	"google.golang.org/protobuf/proto"
)

// PolicyWorker has no transport authority. Calls happen outside account serialization.
type PolicyWorker interface {
	Propose(context.Context, *pb.DecisionRequest) (inference.Proposal, error)
}
type PolicyConfig struct {
	Worker           PolicyWorker
	PolicySHA256     string
	Profile          string
	ExpectedMode     string
	AllowUnvalidated bool
	// Fallback is explicitly pinned as "baseline" or "pause".
	Fallback string
	Timeout  time.Duration
}
type PolicyBundle struct {
	Version                 string `json:"version"`
	PolicySHA256            string `json:"policy_sha256"`
	ObservationVersion      string `json:"observation_version"`
	ObservationSchemaSHA256 string `json:"observation_schema_sha256"`
	PromotionStatus         string `json:"promotion_status"`
	Profile                 string `json:"profile"`
	Fallback                string `json:"fallback"`
	TimeoutNS               int64  `json:"timeout_ns"`
}

// PolicyDecision is a durable request and outcome projection. Slot identifies one
// obligation; its resolution commits in the same transaction as an action enqueue.
type PolicyDecision struct {
	RequestedSourceCursor   *SourceCursor `json:"requested_source_cursor"`
	AppliedSourceCursor     *SourceCursor `json:"applied_source_cursor"`
	RequestID               string        `json:"request_id"`
	Slot                    uint64        `json:"slot"`
	ParentID                string        `json:"parent_id"`
	ParentRevision          uint64        `json:"parent_revision"`
	AuthorityRevision       uint64        `json:"authority_revision"`
	JournalRevision         int64         `json:"journal_revision"`
	OwnerGeneration         int64         `json:"owner_generation"`
	MarketSnapshotID        string        `json:"market_snapshot_id"`
	AppliedMarketSnapshotID string        `json:"applied_market_snapshot_id,omitempty"`
	ExpiresAt               time.Time     `json:"expires_at"`
	Observation             []float32     `json:"observation"`
	Bundle                  PolicyBundle  `json:"bundle"`
	Result                  string        `json:"result"`
	Reason                  string        `json:"reason"`
	Action                  d.Action      `json:"action"`
	ProposedAction          *d.Action     `json:"proposed_action,omitempty"`
	ResolvedAt              time.Time     `json:"resolved_at,omitempty"`
	Mode                    string        `json:"mode,omitempty"`
}
type policyResolution struct {
	audit    PolicyDecision
	baseline bool
}
type policyResult struct {
	audit    PolicyDecision
	proposal inference.Proposal
	err      error
}

func policyBundle(c *PolicyConfig) (*PolicyBundle, error) {
	if c == nil {
		return nil, nil
	}
	if c.ExpectedMode == "" {
		c.ExpectedMode = "unvalidated"
	}
	hash, err := hex.DecodeString(c.PolicySHA256)
	if err != nil || len(hash) != 32 || strings.ToLower(c.PolicySHA256) != c.PolicySHA256 || c.Worker == nil || len(c.ExpectedMode) > 64 || c.ExpectedMode == "live" || !c.AllowUnvalidated || (c.Profile != "paper" && c.Profile != "shadow") || (c.Fallback != "baseline" && c.Fallback != "pause") || c.Timeout <= 0 || c.Timeout > inference.MaxValidity {
		return nil, ErrInvalid
	}
	return &PolicyBundle{Version: "policy-bundle-v1", PolicySHA256: c.PolicySHA256, ObservationVersion: x.ObservationVersion, ObservationSchemaSHA256: inference.SchemaSHA256(), PromotionStatus: c.ExpectedMode, Profile: c.Profile, Fallback: c.Fallback, TimeoutNS: int64(c.Timeout)}, nil
}
func sameBundle(a, b *PolicyBundle) bool { return reflect.DeepEqual(a, b) }

func terminalPolicyParent(p *Parent) bool {
	return p == nil || p.Status == "completed" || p.Status == "cancelled" || p.Status == "expired"
}

// A new bundle belongs to a new session. All old financial obligations must be
// settled before changing the account default; the terminal parent keeps its pin.
func canReplacePolicyBundle(state State, account pg.Account) bool {
	if !terminalPolicyParent(state.Parent) || state.Child != nil || state.PolicyPending != nil || len(state.PendingFacts) != 0 || len(account.Intents) != 0 || len(account.Reservations) != 0 || account.ObservedSequence != account.ProcessedSequence {
		return false
	}
	for _, fact := range state.UnresolvedFacts {
		if !fact.Resolved {
			return false
		}
	}
	return true
}

func (a *Actor) policyBinding(p *PolicyDecision) *pb.DecisionBinding {
	return &pb.DecisionBinding{RequestId: p.RequestID, TenantId: a.config.Key.TenantID, AccountId: a.config.Key.AccountID, SessionId: p.ParentID, SessionRevision: p.ParentRevision, AccountRevision: p.AuthorityRevision, PolicySha256: p.Bundle.PolicySHA256, ObservationVersion: p.Bundle.ObservationVersion, ObservationSchemaSha256: p.Bundle.ObservationSchemaSHA256, MarketSnapshotId: p.MarketSnapshotID, ExpiresAtUnixNs: p.ExpiresAt.UnixNano()}
}
func (a *Actor) startPolicy(ctx context.Context) error {
	if a.pending != nil || a.account.ObservedSequence != a.account.ProcessedSequence {
		return ErrRecoveryRequired
	}
	if a.state.PolicyPending != nil {
		return nil
	}
	started := a.metricStart()
	p := a.state.Parent
	if p == nil || p.PolicyBundle == nil || !sameBundle(p.PolicyBundle, a.state.PolicyBundle) {
		return ErrInvalid
	}
	ledger, err := a.ledger(a.state)
	if err != nil {
		return err
	}
	var child *d.ChildOrder
	if a.state.Child != nil {
		child = &a.state.Child.Order
	}
	mids := append([]d.Decimal(nil), a.state.DecisionMids...)
	if a.state.DecisionCount > 0 {
		mids = append(mids, a.state.Book.Mid())
		if len(mids) > 11 {
			mids = mids[len(mids)-11:]
		}
	}
	observation, err := x.BuildObservation(x.FeatureSnapshot{Plan: p.Spec, Ledger: ledger, Book: a.state.Book, Child: child, Now: a.elapsed(a.state), ReferenceMid: a.state.Book.Mid(), DecisionMids: mids, TradeFlow: a.state.TradeFlow, AccountValid: a.account.Ready})
	if err != nil {
		return err
	}
	expires := a.now().Add(a.config.Policy.Timeout)
	if p.Deadline.Before(expires) {
		expires = p.Deadline
	}
	pending := &PolicyDecision{RequestID: randomID("runtime-policy-"), Slot: a.state.DecisionCount + 1, ParentID: p.ID, ParentRevision: uint64(p.Revision), AuthorityRevision: uint64((View{Account: a.account, State: a.state}).AuthorityRevision()), JournalRevision: a.account.Revision, OwnerGeneration: a.owner.Epoch, MarketSnapshotID: a.state.MarketID, ExpiresAt: expires, Observation: observation, Bundle: *p.PolicyBundle, Result: "pending", Action: d.Hold}
	pending.RequestedSourceCursor = copySourceCursor(a.state.SourceCursor)
	next, err := cloneState(a.state)
	if err != nil {
		return err
	}
	next.PolicyPending = pending
	if _, err = a.persistRevision(ctx, next, pg.Command{ID: pending.RequestID, Kind: "policy_request", ExpectedRevision: a.account.Revision}, pg.Change{RequireCaughtUp: true}, true); err != nil {
		return err
	}
	a.policyStartedAt = started
	faultpoint.Hit("policy_pending")
	a.resetPolicyTimer(pending.ExpiresAt)
	if a.policyBusy {
		return a.resolvePolicy(ctx, "fallback", "worker_busy", nil)
	}
	request := &pb.DecisionRequest{Binding: a.policyBinding(pending), Observation: &pb.Observation{SchemaVersion: x.ObservationVersion, FieldNames: append([]string(nil), x.ObservationFields...), Features: append([]float32(nil), observation...)}}
	callCtx, cancel := context.WithDeadline(context.Background(), expires)
	a.policyBusy = true
	a.policyCancel = cancel
	go func() {
		proposal, err := a.config.Policy.Worker.Propose(callCtx, request)
		cancel()
		select {
		case a.policyResults <- policyResult{audit: *pending, proposal: proposal, err: err}:
		case <-a.done:
		}
	}()
	return nil
}
func policyError(err error) string {
	switch {
	case errors.Is(err, inference.ErrBusy):
		return "worker_busy"
	case errors.Is(err, inference.ErrIncompatible):
		return "incompatible_response"
	case errors.Is(err, inference.ErrInvalid):
		return "invalid_response"
	case errors.Is(err, inference.ErrStale), errors.Is(err, context.DeadlineExceeded):
		return "expired"
	default:
		return "worker_unavailable"
	}
}
func (a *Actor) receivePolicy(result policyResult) {
	a.policyBusy = false
	if a.policyCancel != nil {
		a.policyCancel()
		a.policyCancel = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.refreshOwned(ctx); err != nil {
		return
	}
	pending := a.state.PolicyPending
	if pending == nil || result.audit.RequestID != pending.RequestID {
		next, err := cloneState(a.state)
		if err != nil {
			return
		}
		// The final decision remains intact; this independent entry records the late
		// worker outcome and carries no action authority.
		late := result.audit
		late.Result, late.Reason, late.Action, late.ResolvedAt = "late_response", "slot_already_resolved", d.Hold, a.now()
		if result.err == nil && result.proposal.Action.Validate() == nil {
			proposed := result.proposal.Action
			late.ProposedAction = &proposed
			if len(result.proposal.Mode) <= 64 {
				late.Mode = result.proposal.Mode
			}
		}
		next.PolicyLateOutcome = &late
		if result.err != nil {
			next.PolicyLateOutcome.Reason = policyError(result.err)
		}
		_, _ = a.persistRevision(ctx, next, pg.Command{ID: randomID("runtime-policy-late-"), Kind: "policy_late", ExpectedRevision: a.account.Revision}, pg.Change{}, true)
		return
	}
	// Facts precede eligibility, including observations durably accepted while the
	// RPC was running but not yet consumed by the ordinary actor mailbox. Late
	// outcomes above grant no authority and remain auditable during recovery.
	if a.account.ObservedSequence != a.account.ProcessedSequence {
		_ = a.resolvePolicy(ctx, "fallback", "source_facts_pending", nil)
		return
	}
	if result.err != nil {
		_ = a.resolvePolicy(ctx, "fallback", policyError(result.err), nil)
		return
	}
	if !proto.Equal(result.proposal.Binding, a.policyBinding(pending)) || result.proposal.Mode != pending.Bundle.PromotionStatus {
		_ = a.resolvePolicy(ctx, "fallback", "incompatible_response", nil)
		return
	}
	p := a.state.Parent
	current := inference.Current{TenantID: a.config.Key.TenantID, AccountID: a.config.Key.AccountID, PolicySHA256: a.state.PolicyBundle.PolicySHA256, AccountRevision: uint64((View{Account: a.account, State: a.state}).AuthorityRevision()), Ready: a.account.Ready && !a.state.RecoveryRequired && !a.overflow.Load() && a.account.LeaseUntil.After(a.now()), MarketValid: a.state.Book.Valid}
	if p != nil {
		current.SessionID = p.ID
		current.SessionRevision = uint64(p.Revision)
		current.Active = p.Status == "active" && a.now().Before(p.Deadline)
	}
	if err := result.proposal.ValidateCurrent(current, a.now()); err != nil {
		_ = a.resolvePolicy(ctx, "fallback", "stale_authority_or_expired", nil)
		return
	}
	if a.config.Policy.Profile == "shadow" {
		_ = a.resolvePolicy(ctx, "shadow_baseline", "shadow_policy", &result.proposal)
		return
	}
	_ = a.resolvePolicy(ctx, "policy_action", "accepted", &result.proposal)
}
func (a *Actor) expirePolicy(ctx context.Context) error {
	if p := a.state.PolicyPending; p != nil && !a.now().Before(p.ExpiresAt) {
		if a.policyCancel != nil {
			a.policyCancel()
		}
		return a.resolvePolicy(ctx, "fallback", "expired", nil)
	}
	return nil
}
func (a *Actor) resolvePolicy(ctx context.Context, result, reason string, proposal *inference.Proposal) error {
	pending := a.state.PolicyPending
	if pending == nil {
		return nil
	}
	defer func() {
		if a.state.PolicyPending == nil {
			a.stopPolicyTimer()
		}
	}()
	audit := *pending
	audit.Result = result
	audit.Reason = reason
	audit.ResolvedAt = a.now()
	audit.AppliedMarketSnapshotID = a.state.MarketID
	audit.AppliedSourceCursor = copySourceCursor(a.state.SourceCursor)
	if proposal != nil {
		audit.Mode = proposal.Mode
		audit.Action = proposal.Action
		proposed := proposal.Action
		audit.ProposedAction = &proposed
	}
	p := a.state.Parent
	active := p != nil && p.ID == audit.ParentID && p.Status == "active" && a.now().Before(p.Deadline)
	ready := active && a.pending == nil && a.account.ObservedSequence == a.account.ProcessedSequence && a.account.Ready && a.state.Book.Valid && !a.state.RecoveryRequired && !a.overflow.Load()
	baseline := result == "shadow_baseline" || result == "fallback" && audit.Bundle.Fallback == "baseline"
	if ready && (result == "policy_action" || baseline) {
		if baseline {
			audit.Result = "fallback_baseline"
			if result == "shadow_baseline" {
				audit.Result = result
			}
		}
		_, err := a.command(ctx, Command{ID: audit.RequestID + "-resolve", Kind: "decision", Action: audit.Action, ExpectedRevision: a.account.Revision, policy: &policyResolution{audit: audit, baseline: baseline}})
		if err == nil {
			return nil
		}
		if a.needsReload {
			if reloadErr := a.refreshOwned(ctx); reloadErr != nil {
				return reloadErr
			}
		}
		// Compilation/persistence errors cannot authorize a second action. If the
		// slot was durably consumed, its original outcome already owns the decision.
		if a.state.PolicyPending == nil {
			return err
		}
		reason = "action_rejected"
		audit.Reason = reason
	}
	next, err := cloneState(a.state)
	if err != nil {
		return err
	}
	audit.Result = "fallback_pause"
	audit.Action = d.Hold
	next.PolicyPending = nil
	next.LastPolicyDecision = &audit
	next.DecisionCount = max(next.DecisionCount, audit.Slot)
	next.NextDecision = a.elapsed(next) + next.Execution.DecisionInterval
	change := pg.Change{}
	if active {
		// Pause and its audit are one transaction, before child cancellation.
		_, err = a.command(ctx, Command{ID: audit.RequestID + "-resolve", Kind: "pause", ExpectedRevision: a.account.Revision, policy: &policyResolution{audit: audit}})
		return err
	}
	_, err = a.persistRevision(ctx, next, pg.Command{ID: audit.RequestID + "-resolve", Kind: "policy_fallback", ExpectedRevision: a.account.Revision}, change, true)
	return err
}

// This timer only wakes for pending inference. Lease renewal and normal cadence
// retain their original maintenance frequency.
func (a *Actor) resetPolicyTimer(deadline time.Time) {
	if a.policyTimer == nil {
		return
	}
	a.stopPolicyTimer()
	a.policyTimer.Reset(max(time.Nanosecond, deadline.Sub(a.now())))
	a.policyExpiry = a.policyTimer.C
}
func (a *Actor) stopPolicyTimer() {
	if a.policyTimer == nil {
		return
	}
	if !a.policyTimer.Stop() {
		select {
		case <-a.policyTimer.C:
		default:
		}
	}
	a.policyExpiry = nil
}
func (a *Actor) handlePolicyExpiry() {
	a.policyExpiry = nil
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if a.refreshOwned(ctx) == nil {
		_ = a.expirePolicy(ctx)
	}
	if a.state.PolicyPending != nil {
		a.resetPolicyTimer(a.now().Add(100 * time.Millisecond))
	}
}
