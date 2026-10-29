package api

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"

	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
)

// SourceProvenanceDTO is a curated projection of persisted sealed-source
// metadata. It carries no receipts, paths, raw state or per-receipt payload digests.
// Native elapsed time remains independent of account elapsed and wall clocks.
type SourceProvenanceDTO struct {
	SealedSource         SealedSourceDTO         `json:"sealed_source"`
	StartCursor          *SourceCursorDTO        `json:"start_cursor"`
	CurrentDurableCursor *SourceCursorDTO        `json:"current_durable_cursor"`
	LastDecisionCursor   *SourceCursorDTO        `json:"last_decision_cursor"`
	LatestPolicy         *PolicySourceCursorsDTO `json:"latest_policy,omitempty"`
}

type SealedSourceDTO struct {
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

type SourceCursorDTO struct {
	ArchiveID              string `json:"archive_id"`
	NativeSessionID        string `json:"native_session_id"`
	NormalizedReceiptIndex uint64 `json:"normalized_receipt_index"`
	RawReceiptIndex        uint64 `json:"raw_receipt_index"`
	NormalizedTimeNS       int64  `json:"normalized_time_ns"`
	NativeElapsedNS        int64  `json:"native_elapsed_ns"`
}

type PolicySourceCursorsDTO struct {
	RequestedCursor *SourceCursorDTO `json:"requested_cursor,omitempty"`
	AppliedCursor   *SourceCursorDTO `json:"applied_cursor,omitempty"`
}

func cursorProjection(c *rt.SourceCursor) *SourceCursorDTO {
	if c == nil {
		return nil
	}
	return &SourceCursorDTO{ArchiveID: c.ArchiveID, NativeSessionID: c.NativeSessionID, NormalizedReceiptIndex: c.NormalizedReceiptIndex, RawReceiptIndex: c.RawReceiptIndex, NormalizedTimeNS: c.NormalizedTimeNS, NativeElapsedNS: c.NativeElapsedNS}
}

func validSourceCursor(source *rt.SealedSource, cursor *rt.SourceCursor) bool {
	return source != nil && cursor != nil && cursor.ArchiveID == source.ArchiveID && cursor.NativeSessionID == source.NativeSessionID && cursor.NormalizedReceiptIndex < source.NormalizedEvents && cursor.RawReceiptIndex >= source.FirstSnapshotRawReceiptIndex && cursor.RawReceiptIndex < source.RawReceipts-1 && cursor.NormalizedTimeNS >= 0 && cursor.NormalizedTimeNS <= source.CoverageEndNS && cursor.NativeElapsedNS == source.FirstSnapshotElapsedNS+cursor.NormalizedTimeNS
}

func sourceCursorOrdered(before, after *rt.SourceCursor) bool {
	if before == nil {
		return true
	}
	if after == nil {
		return false
	}
	if before.NormalizedReceiptIndex == after.NormalizedReceiptIndex {
		return reflect.DeepEqual(before, after)
	}
	return before.NormalizedReceiptIndex < after.NormalizedReceiptIndex && before.RawReceiptIndex <= after.RawReceiptIndex && before.NormalizedTimeNS <= after.NormalizedTimeNS && before.NativeElapsedNS <= after.NativeElapsedNS
}

func sourceProvenance(s rt.State) (*SourceProvenanceDTO, error) {
	if err := s.ValidateSource(); err != nil {
		return nil, rt.ErrInvalid
	}
	if s.SealedSource == nil {
		for _, audit := range []*rt.PolicyDecision{s.PolicyPending, s.LastPolicyDecision} {
			if audit != nil && (audit.RequestedSourceCursor != nil || audit.AppliedSourceCursor != nil) {
				return nil, rt.ErrInvalid
			}
		}
		return nil, nil
	}
	source := s.SealedSource
	// The archive ID pins the exact raw/source-manifest hashes and normalizer
	// version. Merely well-shaped but inconsistent hashes cannot be published.
	id := sha256.Sum256([]byte(marketarchive.Schema + "\n" + source.Raw.SHA256 + "\n" + source.SourceManifest.SHA256 + "\n" + source.NormalizationVersion + "\n"))
	if source.ArchiveID != hex.EncodeToString(id[:]) || s.Parent == nil || s.Parent.Spec.Symbol != source.Symbol {
		return nil, rt.ErrInvalid
	}
	if s.Parent.SealedSource != nil && !reflect.DeepEqual(s.Parent.SealedSource, source) {
		return nil, rt.ErrInvalid
	}
	if !s.Parent.Started && s.Parent.SourceStartCursor != nil {
		return nil, rt.ErrInvalid
	}
	if !sourceCursorOrdered(s.Parent.SourceStartCursor, s.SourceCursor) || !sourceCursorOrdered(s.LastDecisionSourceCursor, s.SourceCursor) {
		return nil, rt.ErrInvalid
	}
	if s.LastDecisionSourceCursor != nil && !sourceCursorOrdered(s.Parent.SourceStartCursor, s.LastDecisionSourceCursor) {
		return nil, rt.ErrInvalid
	}
	for _, audit := range []*rt.PolicyDecision{s.PolicyPending, s.LastPolicyDecision} {
		if audit == nil {
			continue
		}
		for _, cursor := range []*rt.SourceCursor{audit.RequestedSourceCursor, audit.AppliedSourceCursor} {
			if cursor != nil && (audit.ParentID != s.Parent.ID || !validSourceCursor(source, cursor) || !sourceCursorOrdered(s.Parent.SourceStartCursor, cursor) || !sourceCursorOrdered(cursor, s.SourceCursor)) {
				return nil, rt.ErrInvalid
			}
		}
		if !sourceCursorOrdered(audit.RequestedSourceCursor, audit.AppliedSourceCursor) && audit.AppliedSourceCursor != nil {
			return nil, rt.ErrInvalid
		}
	}
	out := &SourceProvenanceDTO{
		SealedSource: SealedSourceDTO{ArchiveID: source.ArchiveID, Raw: source.Raw, SourceManifest: source.SourceManifest, Normalized: source.Normalized, NormalizationVersion: source.NormalizationVersion, NativeSessionID: source.NativeSessionID, Symbol: source.Symbol, FirstSnapshotElapsedNS: source.FirstSnapshotElapsedNS, FirstSnapshotRawReceiptIndex: source.FirstSnapshotRawReceiptIndex, CoverageEndNS: source.CoverageEndNS, NormalizedEvents: source.NormalizedEvents, RawReceipts: source.RawReceipts},
		StartCursor:  cursorProjection(s.Parent.SourceStartCursor), CurrentDurableCursor: cursorProjection(s.SourceCursor), LastDecisionCursor: cursorProjection(s.LastDecisionSourceCursor),
	}
	audit := s.LastPolicyDecision
	if s.PolicyPending != nil {
		audit = s.PolicyPending
	}
	if audit != nil && (audit.RequestedSourceCursor != nil || audit.AppliedSourceCursor != nil) {
		out.LatestPolicy = &PolicySourceCursorsDTO{RequestedCursor: cursorProjection(audit.RequestedSourceCursor), AppliedCursor: cursorProjection(audit.AppliedSourceCursor)}
	}
	return out, nil
}

func matchingSourceReference(current, reference rt.State) bool {
	if !reflect.DeepEqual(current.SealedSource, reference.SealedSource) {
		return false
	}
	if current.SealedSource == nil {
		return true
	}
	return current.Parent != nil && reference.Parent != nil && reflect.DeepEqual(current.Parent.SealedSource, reference.Parent.SealedSource) && reflect.DeepEqual(current.Parent.SourceStartCursor, reference.Parent.SourceStartCursor) && sourceCursorOrdered(reference.SourceCursor, current.SourceCursor)
}
