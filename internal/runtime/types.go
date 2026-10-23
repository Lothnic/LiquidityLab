// Package runtime owns one serialized, durable account authority. Its implemented
// transport profile is paper only; live transport construction is unavailable.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

type Profile string

const (
	ProfileReplay Profile = "replay"
	ProfilePaper  Profile = "paper"
	ProfileLive   Profile = "live"
)
const StateVersion = "account-state-v1"

var (
	ErrProfile          = errors.New("account authority supports the paper profile only; live transport is unavailable")
	ErrMailboxFull      = errors.New("account mailbox full; caller must retain and retry unaccepted facts")
	ErrRecoveryRequired = errors.New("account requires reconciliation before new sends")
	ErrClosed           = errors.New("account authority is closed")
	ErrOutcomeUnknown   = errors.New("accepted command outcome unavailable; retry identical identity")
	ErrInvalid          = errors.New("invalid account command or state")
	ErrHistoryLimit     = errors.New("account state history limit reached")
)

type Config struct {
	DataSource                                                         string
	AutoExecute                                                        bool
	Baseline                                                           string
	Store                                                              *pg.Store
	Key                                                                pg.Key
	Profile                                                            Profile
	OwnerID                                                            string
	Lease                                                              time.Duration
	MailboxCapacity, ControlCapacity, FactCapacity, MaxFills, MaxFacts int
	Rules                                                              d.InstrumentRules
	Fees                                                               d.FeeConfig
	Execution                                                          d.ReplayConfig
	BaseCurrency, QuoteCurrency, InstrumentRevision                    string
	RecoveryProof                                                      pg.RecoveryProof
}

func DefaultConfig(store *pg.Store, key pg.Key) Config {
	return Config{Store: store, Key: key, Profile: ProfilePaper, Lease: time.Minute, MailboxCapacity: 64, ControlCapacity: 16, FactCapacity: 64, MaxFills: 2048, MaxFacts: 256, Rules: d.DefaultRules(), Fees: d.DefaultFees(), Execution: d.DefaultConfig(), BaseCurrency: "BTC", QuoteCurrency: "USD", InstrumentRevision: "synthetic-rules-v1", Baseline: x.BaselineScheduled}
}

type Parent struct {
	ID         string    `json:"id"`
	Revision   int64     `json:"revision"`
	Status     string    `json:"status"`
	Started    bool      `json:"started"`
	Start      time.Time `json:"start"`
	Deadline   time.Time `json:"deadline"`
	Spec       d.Plan    `json:"spec"`
	Baseline   string    `json:"baseline"`
	ArrivalMid d.Decimal `json:"arrival_mid"`
}
type Child struct {
	ID          string       `json:"id"`
	Action      d.Action     `json:"action"`
	Order       d.ChildOrder `json:"order"`
	State       string       `json:"state"`
	KnownFinal  bool         `json:"known_final"`
	FinalReason string       `json:"final_reason"`
}
type Fact struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id,omitempty"`
	IntentID string `json:"intent_id"`
	Fill     d.Fill `json:"fill"`
	Reason   string `json:"reason,omitempty"`
	Resolved bool   `json:"resolved"`
}
type State struct {
	Version string `json:"version"`
	// AuthorityRevision is the last durable revision that changed financial or
	// control authority. Source receipts can advance the journal independently.
	AuthorityRevision  int64             `json:"authority_revision"`
	Profile            Profile           `json:"profile"`
	DataSource         string            `json:"data_source"`
	Rules              d.InstrumentRules `json:"rules"`
	Fees               d.FeeConfig       `json:"fees"`
	Execution          d.ReplayConfig    `json:"execution"`
	BaseCurrency       string            `json:"base_currency"`
	QuoteCurrency      string            `json:"quote_currency"`
	InstrumentRevision string            `json:"instrument_revision"`
	Parent             *Parent           `json:"parent,omitempty"`
	Child              *Child            `json:"child,omitempty"`
	Fills              []d.Fill          `json:"fills"`
	Book               *x.Book           `json:"book"`
	MarketID           string            `json:"market_id"`
	RecoveryRequired   bool              `json:"recovery_required"`
	ClockDiscontinuity bool              `json:"clock_discontinuity"`
	Reason             string            `json:"reason"`
	PendingFacts       []Fact            `json:"pending_facts"`
	UnresolvedFacts    []Fact            `json:"unresolved_facts"`
	LastInputID        string            `json:"last_input_id"`
	LastInputDigest    string            `json:"last_input_digest"`
	LastMessage        int64             `json:"last_message_ns"`
	HasMessage         bool              `json:"has_message"`
	TradeFlow          d.Decimal         `json:"trade_flow"`
	DecisionMids       []d.Decimal       `json:"decision_mids"`
	ReferenceMid       d.Decimal         `json:"reference_mid"`
	NextSlice          int64             `json:"next_slice_ns"`
	NextDecision       int64             `json:"next_decision_ns"`
	DecisionCount      uint64            `json:"decision_count"`
	LastObservation    []float32         `json:"last_observation"`
}
type Command struct {
	ID                        string    `json:"id"`
	ExpectedRevision          int64     `json:"expected_revision"`
	ExpectedAuthorityRevision *int64    `json:"expected_authority_revision,omitempty"`
	Kind                      string    `json:"kind"`
	PlanID                    string    `json:"plan_id,omitempty"`
	Plan                      *d.Plan   `json:"plan,omitempty"`
	Deadline                  time.Time `json:"deadline,omitempty"`
	Action                    d.Action  `json:"action,omitempty"`
	IntentID                  string    `json:"intent_id,omitempty"`
	Policy                    string    `json:"policy,omitempty"`
}
type View struct {
	Account pg.Account
	State   State
}

// AuthorityRevision falls back conservatively for snapshots written before the
// authority marker existed. New actors initialize it at their bootstrap commit.
func (v View) AuthorityRevision() int64 {
	if v.State.AuthorityRevision <= 0 {
		return v.Account.Revision
	}
	return v.State.AuthorityRevision
}

type Reconciliation struct {
	ID               string
	ExpectedRevision int64
	Evidence         string
	NoExternalOrders bool
	Balances         []pg.Balance
	Resolutions      []pg.Resolution
	ResolvedFactIDs  []string
	AbortParent      bool
}

type HandoffState uint8

const (
	RejectedBeforeHandoff HandoffState = iota
	PotentiallyInFlight
)

type Submission struct {
	ID                       string
	Intent                   d.OrderIntent
	Book                     *x.Book
	Fees                     d.FeeConfig
	Now                      int64
	OperationID, InputDigest string
}
type TransportResult struct {
	Handoff              HandoffState
	Accepted, KnownFinal bool
	VenueOrderID, Reason string
	Fills                []d.Fill
	QueueAhead           d.Decimal
}
type transportFact struct {
	IntentID    string          `json:"intent_id"`
	OperationID string          `json:"operation_id"`
	InputDigest string          `json:"input_digest"`
	Outcome     TransportResult `json:"outcome"`
	Intent      *d.OrderIntent  `json:"intent,omitempty"`
}

// Handoff returns after transport owns the request or rejects it definitively.
// PotentiallyInFlight is required whenever acceptance cannot be ruled out.
type Transport interface {
	Profile() Profile
	Handoff(context.Context, Submission) (TransportResult, error)
	Cancel(context.Context, string) (TransportResult, error)
}
type response struct {
	result pg.Result
	view   View
	err    error
}
type request struct {
	sequence uint64
	kind     string
	input    any
	ctx      context.Context
	reply    chan response
}
type marketInput struct {
	Event      d.MarketEvent
	SnapshotID string
	SessionID  string
}
type fillInput struct {
	ID, IntentID string
	Fill         d.Fill
}

func jsonBytes(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
