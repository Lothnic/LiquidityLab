// Package postgres owns durable account transitions. It performs no venue or
// inference network calls and never treats a database epoch as exchange fencing.
package postgres

import (
	"encoding/json"
	"errors"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

var (
	ErrNotFound          = errors.New("account or durable object not found")
	ErrConflict          = errors.New("identity conflicts with an existing durable operation")
	ErrStaleRevision     = errors.New("stale account or plan revision")
	ErrOwnership         = errors.New("ownership epoch or lease is not current")
	ErrIsolationRequired = errors.New("prior sender isolation and reconciliation evidence required")
	ErrNotReady          = errors.New("account or market is not ready")
	ErrInsufficientFunds = errors.New("unreflected reservations exceed free funds")
	ErrExpired           = errors.New("plan or intent has expired")
	ErrInvalid           = errors.New("invalid durable transition")
	ErrSchema            = errors.New("migration version or checksum mismatch")
)

type Key struct {
	TenantID  string `json:"tenant_id"`
	AccountID string `json:"account_id"`
}
type Ownership struct {
	OwnerID string `json:"owner_id"`
	Epoch   int64  `json:"epoch"`
}
type Balance struct {
	Currency string    `json:"currency"`
	Total    d.Decimal `json:"total"`
	Free     d.Decimal `json:"free"`
	Locked   d.Decimal `json:"locked"`
}
type Market struct {
	Valid              bool   `json:"valid"`
	SnapshotID         string `json:"snapshot_id"`
	InstrumentRevision string `json:"instrument_revision"`
}
type Plan struct {
	ID       string          `json:"id"`
	Revision int64           `json:"revision"`
	Status   string          `json:"status"`
	Deadline time.Time       `json:"deadline"`
	Detail   json.RawMessage `json:"detail"`
}
type Reservation struct {
	IntentID  string    `json:"intent_id"`
	Currency  string    `json:"currency"`
	Amount    d.Decimal `json:"amount"`
	Reflected bool      `json:"reflected"`
	State     string    `json:"state"`
}

// ReservationAdjustment reduces remaining exposure after a newly recorded fill.
// Zero exposure is released only through a known-final Resolution.
type ReservationAdjustment struct {
	IntentID  string    `json:"intent_id"`
	Currency  string    `json:"currency"`
	Remaining d.Decimal `json:"remaining"`
}
type Intent struct {
	ID                    string          `json:"id"`
	PlanID                string          `json:"plan_id"`
	PlanRevision          int64           `json:"plan_revision"`
	ExpiresAt             time.Time       `json:"expires_at"`
	InstrumentRevision    string          `json:"instrument_revision"`
	Payload               json.RawMessage `json:"payload"`
	State                 string          `json:"state,omitempty"`
	AttemptID             string          `json:"attempt_id,omitempty"`
	VenueOrderID          string          `json:"venue_order_id,omitempty"`
	AuthorizationRevision int64           `json:"authorization_revision,omitempty"`
	AuthorizationSnapshot string          `json:"authorization_snapshot,omitempty"`
}
type Resolution struct {
	IntentID     string          `json:"intent_id"`
	State        string          `json:"state"`
	VenueOrderID string          `json:"venue_order_id"`
	KnownFinal   bool            `json:"known_final"`
	Evidence     json.RawMessage `json:"evidence"`
	Reflected    *bool           `json:"reflected,omitempty"`
}
type Execution struct {
	ID       string          `json:"id"`
	IntentID string          `json:"intent_id"`
	Venue    string          `json:"venue"`
	Detail   json.RawMessage `json:"detail"`
}
type Command struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	ExpectedRevision int64  `json:"expected_revision"`
}

// Change is one atomic account transition. Plan revisions begin at one and must
// increment exactly once. Balance entries replace their currencies' projections.
// A queued intent must carry a positive, initially unreflected reservation.
// Known-final resolutions release reservations; unknown outcomes retain them.
// Snapshot contains the caller's versioned reducer state, never credentials.
type Change struct {
	Key                Key                     `json:"key"`
	Ownership          Ownership               `json:"ownership"`
	Command            Command                 `json:"command"`
	Snapshot           json.RawMessage         `json:"snapshot,omitempty"`
	Ready              *bool                   `json:"ready,omitempty"`
	Market             *Market                 `json:"market,omitempty"`
	Balances           []Balance               `json:"balances,omitempty"`
	Plan               *Plan                   `json:"plan,omitempty"`
	Enqueue            []Intent                `json:"enqueue,omitempty"`
	Reservations       []Reservation           `json:"reservations,omitempty"`
	ReservationUpdates []ReservationAdjustment `json:"reservation_updates,omitempty"`
	Resolutions        []Resolution            `json:"resolutions,omitempty"`
	Execution          *Execution              `json:"execution,omitempty"`
}
type Result struct {
	Revision  int64 `json:"revision"`
	Duplicate bool  `json:"duplicate"`
}
type Account struct {
	Key          Key             `json:"key"`
	Revision     int64           `json:"revision"`
	Ownership    Ownership       `json:"ownership"`
	LeaseUntil   time.Time       `json:"lease_until"`
	Ready        bool            `json:"ready"`
	Market       Market          `json:"market"`
	Snapshot     json.RawMessage `json:"snapshot"`
	Balances     []Balance       `json:"balances"`
	Plans        []Plan          `json:"plans"`
	Reservations []Reservation   `json:"reservations"`
	Intents      []Intent        `json:"intents"`
}

// RecoveryProof records an operational assertion. The database cannot verify that
// a prior process has lost its venue connection, nor fence bytes already sent.
type RecoveryProof struct {
	PreviousSenderIsolated bool   `json:"previous_sender_isolated"`
	Reconciled             bool   `json:"reconciled"`
	Reference              string `json:"reference"`
}
type Authorization struct {
	Key                Key
	Ownership          Ownership
	IntentID           string
	ExpectedRevision   int64
	PlanRevision       int64
	SnapshotID         string
	InstrumentRevision string
	AttemptID          string
}
type Dispatch struct {
	Intent   Intent
	Revision int64
}
