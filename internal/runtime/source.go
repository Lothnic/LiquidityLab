package runtime

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"

	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

// SealedSource describes an operator-verified immutable archive. Construction
// does not authenticate a venue; callers must obtain the manifest through Load.
// Native elapsed time is the capture clock, independent of account/wall time.
type SealedSource struct {
	ArchiveID                    string                    `json:"archive_id"`
	Raw                          marketarchive.Fingerprint `json:"raw"`
	SourceManifest               marketarchive.Fingerprint `json:"source_manifest"`
	Normalized                   marketarchive.Fingerprint `json:"normalized"`
	NormalizationVersion         string                    `json:"normalization_version"`
	NativeSessionID              string                    `json:"native_session_id"`
	Symbol                       string                    `json:"symbol"`
	FirstSnapshotElapsedNS       int64                     `json:"first_snapshot_elapsed_ns"`
	FirstSnapshotRawReceiptIndex uint64                    `json:"first_snapshot_raw_receipt_index"`
	CoverageEndNS                int64                     `json:"coverage_end_ns"`
	NormalizedEvents             uint64                    `json:"normalized_events"`
	RawReceipts                  uint64                    `json:"raw_receipts"`
}

type SourceCursor struct {
	ArchiveID              string `json:"archive_id"`
	NativeSessionID        string `json:"native_session_id"`
	NormalizedReceiptIndex uint64 `json:"normalized_receipt_index"`
	RawReceiptIndex        uint64 `json:"raw_receipt_index"`
	NormalizedTimeNS       int64  `json:"normalized_time_ns"`
	NativeElapsedNS        int64  `json:"native_elapsed_ns"`
}

func SealedSourceFromManifest(m marketarchive.Manifest) (*SealedSource, error) {
	if m.Schema != marketarchive.Schema {
		return nil, ErrInvalid
	}
	s := &SealedSource{ArchiveID: m.ID, Raw: m.Raw, SourceManifest: m.SourceManifest, Normalized: m.Normalized, NormalizationVersion: m.NormalizationVersion, NativeSessionID: m.Source.SessionID, Symbol: m.Source.Symbol, FirstSnapshotElapsedNS: m.FirstSnapshotElapsedNS, FirstSnapshotRawReceiptIndex: m.FirstSnapshotSourceReceiptIndex, CoverageEndNS: m.CoverageEndNS, NormalizedEvents: m.EventCount, RawReceipts: m.Receipts}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SealedSource) validate() error {
	if s == nil {
		return nil
	}
	if strings.ContainsAny(s.NativeSessionID+s.Symbol, "\r\n\x00") {
		return ErrInvalid
	}
	if !marketarchive.ValidID(s.ArchiveID) || !marketarchive.ValidID(s.Raw.SHA256) || !marketarchive.ValidID(s.SourceManifest.SHA256) || !marketarchive.ValidID(s.Normalized.SHA256) || s.Raw.Bytes <= 0 || s.Raw.Bytes > marketarchive.MaxRawBytes || s.SourceManifest.Bytes <= 0 || s.SourceManifest.Bytes > marketarchive.MaxManifestBytes || s.Normalized.Bytes <= 0 || s.Normalized.Bytes > marketarchive.MaxNormalizedBytes || s.NormalizationVersion != marketarchive.NormalizationVersion || len(s.NativeSessionID) == 0 || len(s.NativeSessionID) > 128 || s.Symbol == "" || len(s.Symbol) > 64 || s.FirstSnapshotElapsedNS < 0 || s.CoverageEndNS < 0 || s.CoverageEndNS > math.MaxInt64-s.FirstSnapshotElapsedNS || s.NormalizedEvents == 0 || s.NormalizedEvents > marketarchive.MaxEvents || s.RawReceipts < 2 || s.FirstSnapshotRawReceiptIndex >= s.RawReceipts-1 {
		return ErrInvalid
	}
	return nil
}

func copySource(s *SealedSource) *SealedSource {
	if s == nil {
		return nil
	}
	out := *s
	return &out
}
func copySourceCursor(c *SourceCursor) *SourceCursor {
	if c == nil {
		return nil
	}
	out := *c
	return &out
}

func validateSourceState(s State) error {
	if err := s.SealedSource.validate(); err != nil {
		return err
	}
	if s.SealedSource == nil {
		if s.SourceCursor != nil || s.SourceReceiptDigest != "" || s.LastDecisionSourceCursor != nil {
			return ErrInvalid
		}
		if s.Parent != nil && (s.Parent.SealedSource != nil || s.Parent.SourceStartCursor != nil) {
			return ErrInvalid
		}
		return nil
	}
	if s.DataSource != "recorded" || s.SealedSource.Symbol != s.Rules.Symbol {
		return ErrInvalid
	}
	if s.SourceCursor != nil {
		if err := validateCursor(s.SealedSource, s.SourceCursor); err != nil {
			return err
		}
		if !marketarchive.ValidID(s.SourceReceiptDigest) {
			return ErrInvalid
		}
	} else if s.SourceReceiptDigest != "" {
		return ErrInvalid
	}
	if s.Parent != nil && s.Parent.Started {
		if !reflect.DeepEqual(s.Parent.SealedSource, s.SealedSource) || s.Parent.SourceStartCursor == nil || s.SourceCursor == nil {
			return ErrInvalid
		}
		if err := validateCursor(s.SealedSource, s.Parent.SourceStartCursor); err != nil {
			return err
		}
		if s.Parent.SourceStartCursor.NormalizedReceiptIndex > s.SourceCursor.NormalizedReceiptIndex {
			return ErrInvalid
		}
	}
	if s.LastDecisionSourceCursor != nil {
		if err := validateCursor(s.SealedSource, s.LastDecisionSourceCursor); err != nil {
			return err
		}
		if s.SourceCursor == nil || s.LastDecisionSourceCursor.NormalizedReceiptIndex > s.SourceCursor.NormalizedReceiptIndex {
			return ErrInvalid
		}
	}
	return nil
}

// ValidateSource checks persisted source metadata without changing account or
// market authority. Legacy states with no binding remain explicitly unbound.
func (s State) ValidateSource() error { return validateSourceState(s) }

func validateCursor(s *SealedSource, c *SourceCursor) error {
	if s == nil || c == nil || c.ArchiveID != s.ArchiveID || c.NativeSessionID != s.NativeSessionID || c.NormalizedReceiptIndex >= s.NormalizedEvents || c.RawReceiptIndex < s.FirstSnapshotRawReceiptIndex || c.RawReceiptIndex >= s.RawReceipts-1 || c.NormalizedTimeNS < 0 || c.NormalizedTimeNS > s.CoverageEndNS || c.NativeElapsedNS != s.FirstSnapshotElapsedNS+c.NormalizedTimeNS {
		return ErrInvalid
	}
	return nil
}

func sourceInputDigest(input marketInput) string {
	return inputDigest(struct {
		Event  any
		Source *SourceCursor
	}{input.Event, input.Source})
}

// checkSourceReceipt validates only provenance. It never mutates financial or
// source state. Older trades require separate proof of a frozen durable outcome.
func checkSourceReceipt(state State, input marketInput) (olderTrade bool, err error) {
	s, c := state.SealedSource, input.Source
	if s == nil {
		if c != nil {
			return false, ErrInvalid
		}
		return false, nil
	}
	if err := validateCursor(s, c); err != nil {
		return false, err
	}
	if input.SessionID != s.NativeSessionID || input.Event.Symbol != s.Symbol || input.Event.ReceiptIndex != c.NormalizedReceiptIndex || input.Event.Time != c.NormalizedTimeNS {
		return false, ErrInvalid
	}
	previous := state.SourceCursor
	if previous == nil {
		if c.NormalizedReceiptIndex != 0 || c.RawReceiptIndex != s.FirstSnapshotRawReceiptIndex || c.NormalizedTimeNS != 0 || input.Event.Kind != "snapshot" || input.Event.Invalid {
			return false, ErrInvalid
		}
		return false, nil
	}
	if c.NormalizedReceiptIndex < previous.NormalizedReceiptIndex {
		if input.Event.Kind == "trade" {
			return true, nil
		}
		return false, ErrInvalid
	}
	if c.NormalizedReceiptIndex == previous.NormalizedReceiptIndex {
		if !reflect.DeepEqual(c, previous) || sourceInputDigest(input) != state.SourceReceiptDigest {
			return false, pg.ErrConflict
		}
		return false, nil
	}
	if c.NormalizedReceiptIndex != previous.NormalizedReceiptIndex+1 || c.RawReceiptIndex < previous.RawReceiptIndex || c.NormalizedTimeNS < previous.NormalizedTimeNS {
		return false, ErrInvalid
	}
	return false, nil
}

func acceptSourceReceipt(state *State, input marketInput) {
	if state.SealedSource == nil {
		return
	}
	state.SourceCursor = copySourceCursor(input.Source)
	state.SourceReceiptDigest = sourceInputDigest(input)
}

// Bootstrap and uncertain reload invalidate the book. An exact retry of the
// latest accepted trade must still discover and drain its frozen observation;
// a missing fresh snapshot cannot turn that financial obligation into a no-op.
// checkSourceReceipt must already have established exact latest payload identity.
func sourceNeedsFrozenTradeRetry(state State, input marketInput) bool {
	return state.SealedSource != nil && state.SourceCursor != nil && input.Source != nil && input.Event.Kind == "trade" && !state.Book.Valid && input.Source.NormalizedReceiptIndex == state.SourceCursor.NormalizedReceiptIndex
}

func (a *Actor) rejectSource(ctx context.Context, cause error) error {
	if err := a.invalidateMarket(ctx, "sealed_source_provenance_invalid"); err != nil {
		return err
	}
	return errors.Join(ErrRecoveryRequired, cause)
}

// Accepted trade retries retain their original frozen effect. Looking them up
// cannot reapply an old book or rematch a trade against a different child.
// This proves the economic identity, not an older archive cursor's exact event
// mapping; that mapping is supplied by the trusted verified archive publisher.
func (a *Actor) retryFrozenSourceTrade(ctx context.Context, input marketInput) error {
	id, digest := paperTradeIdentity(input.SessionID, input.Event)
	p, ok := a.transport.(*paperTransport)
	if !ok {
		return ErrInvalid
	}
	fact, found, err := p.storedTrade(ctx, id, digest)
	if err != nil {
		if errors.Is(err, pg.ErrConflict) {
			return a.rejectSource(ctx, err)
		}
		return err
	}
	if !found || !frozenSourceTradeMatches(a.state, input, fact) {
		return a.rejectSource(ctx, ErrInvalid)
	}
	observation, err := p.store.GetObservation(ctx, p.key, id)
	if err != nil {
		return err
	}
	a.wakeFacts()
	if op := a.currentOperation; op != nil {
		op.stage, op.target = operationAwaitFacts, observation.Sequence
		return errFactsPending
	}
	return a.drainFacts(ctx)
}

func frozenSourceTradeMatches(state State, input marketInput, fact paperTradeFact) bool {
	if state.SealedSource == nil || fact.SourceID != state.SealedSource.NativeSessionID {
		return false
	}
	id, digest := paperTradeIdentity(input.SessionID, input.Event)
	return fact.ID == id && fact.InputDigest == digest
}

// A reload may recover an older snapshot after an uncertain transaction while
// newer accepted source books existed only in RAM. Keep the observed cursor,
// but quarantine that book rather than merge financial debt from RAM or claim
// that the durable older book represents the newer receipt. Accepted frozen
// observations can still reduce. Automatic source replay/resume is not provided.
func restoreSourceAfterReload(durable *State, prior State) bool {
	if prior.SealedSource == nil || prior.SourceCursor == nil || !reflect.DeepEqual(prior.SealedSource, durable.SealedSource) {
		return false
	}
	if durable.SourceCursor != nil && durable.SourceCursor.NormalizedReceiptIndex >= prior.SourceCursor.NormalizedReceiptIndex {
		return false
	}
	durable.SourceCursor = copySourceCursor(prior.SourceCursor)
	durable.SourceReceiptDigest = prior.SourceReceiptDigest
	durable.Book.Valid = false
	durable.RecoveryRequired = true
	durable.Reason = "sealed_source_snapshot_required_after_reload"
	return true
}
