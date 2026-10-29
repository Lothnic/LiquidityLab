package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
	"github.com/liquiditylab/liquiditylab/internal/marketdata"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func provenanceFixture(t *testing.T) (rt.State, []marketdata.Event) {
	t.Helper()
	root := t.TempDir()
	base := "../../examples/recordings/kraken-btcusd-20261028/"
	m, err := marketarchive.Import(context.Background(), marketarchive.Config{ArchiveRoot: root, RawPath: base + "public-capture.jsonl", ManifestPath: base + "public-capture.manifest.json"})
	if err != nil {
		t.Fatal(err)
	}
	recording, err := marketarchive.Load(context.Background(), root, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	source, err := rt.SealedSourceFromManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	fills := []d.Fill{{ExecutionID: "fill-1", OrderID: "order-1", Side: "buy", Quantity: d.MustDecimal(".5"), Price: d.MustDecimal("101"), Fee: d.MustDecimal(".01"), FeeCurrency: "base", Time: 150, Maker: true}}
	s, err := rt.DecodeSnapshot(sessionState(t, "bound-plan", "completed", "buy", "100", "110", fills))
	if err != nil {
		t.Fatal(err)
	}
	s.DataSource = "recorded"
	s.SealedSource = source
	s.Parent.SealedSource = cloneSealedSource(source)
	s.Parent.SourceStartCursor = provenanceCursor(source, recording.Events[0])
	s.SourceCursor = provenanceCursor(source, recording.Events[5])
	s.LastDecisionSourceCursor = provenanceCursor(source, recording.Events[3])
	s.SourceReceiptDigest = strings.Repeat("d", 64)
	s.LastPolicyDecision = &rt.PolicyDecision{ParentID: s.Parent.ID, RequestedSourceCursor: provenanceCursor(source, recording.Events[2]), AppliedSourceCursor: provenanceCursor(source, recording.Events[3])}
	return s, recording.Events
}

func cloneSealedSource(source *rt.SealedSource) *rt.SealedSource {
	if source == nil {
		return nil
	}
	out := *source
	return &out
}
func provenanceCursor(source *rt.SealedSource, event marketdata.Event) *rt.SourceCursor {
	return &rt.SourceCursor{ArchiveID: source.ArchiveID, NativeSessionID: source.NativeSessionID, NormalizedReceiptIndex: event.ReceiptIndex, RawReceiptIndex: event.SourceReceiptIndex, NormalizedTimeNS: event.Time, NativeElapsedNS: source.FirstSnapshotElapsedNS + event.Time}
}
func provenanceSaved(t *testing.T, s rt.State) pg.SessionSnapshot {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return pg.SessionSnapshot{Revision: 19, AsOfRevision: 23, CreatedRevision: 2, Snapshot: b}
}

func TestSessionSourceProvenanceExactProjectionAndFinancialPreservation(t *testing.T) {
	s, _ := provenanceFixture(t)
	report, err := buildSessionReport(keyA, s.Parent.ID, provenanceSaved(t, s))
	if err != nil {
		t.Fatal(err)
	}
	p := report.SourceProvenance
	if p == nil || p.SealedSource.ArchiveID != s.SealedSource.ArchiveID || p.SealedSource.Raw != s.SealedSource.Raw || p.SealedSource.SourceManifest != s.SealedSource.SourceManifest || p.SealedSource.Normalized != s.SealedSource.Normalized || p.SealedSource.NormalizationVersion != s.SealedSource.NormalizationVersion || p.SealedSource.NativeSessionID != s.SealedSource.NativeSessionID || p.SealedSource.Symbol != s.SealedSource.Symbol || p.SealedSource.FirstSnapshotElapsedNS != s.SealedSource.FirstSnapshotElapsedNS || p.SealedSource.FirstSnapshotRawReceiptIndex != s.SealedSource.FirstSnapshotRawReceiptIndex || p.SealedSource.CoverageEndNS != s.SealedSource.CoverageEndNS || p.SealedSource.NormalizedEvents != 45 || p.SealedSource.RawReceipts != 51 {
		t.Fatalf("sealed source projection lost metadata: %+v", p)
	}
	if !reflect.DeepEqual(p.StartCursor, cursorProjection(s.Parent.SourceStartCursor)) || !reflect.DeepEqual(p.CurrentDurableCursor, cursorProjection(s.SourceCursor)) || !reflect.DeepEqual(p.LastDecisionCursor, cursorProjection(s.LastDecisionSourceCursor)) || p.LatestPolicy == nil || !reflect.DeepEqual(p.LatestPolicy.RequestedCursor, cursorProjection(s.LastPolicyDecision.RequestedSourceCursor)) || !reflect.DeepEqual(p.LatestPolicy.AppliedCursor, cursorProjection(s.LastPolicyDecision.AppliedSourceCursor)) {
		t.Fatal("source cursor projection changed exact receipt identity")
	}
	legacy := s
	legacy.SealedSource = nil
	legacy.SourceCursor = nil
	legacy.SourceReceiptDigest = ""
	legacy.LastDecisionSourceCursor = nil
	parent := *s.Parent
	parent.SealedSource = nil
	parent.SourceStartCursor = nil
	legacy.Parent = &parent
	legacy.LastPolicyDecision = nil
	baseline, err := buildSessionReport(keyA, legacy.Parent.ID, provenanceSaved(t, legacy))
	if err != nil {
		t.Fatal(err)
	}
	report.SourceProvenance = nil
	if !reflect.DeepEqual(report, baseline) {
		t.Fatal("source projection changed financial fields, status, or report revisions")
	}
	if report.Revision != 19 || report.AsOfRevision != 23 || !report.KnownFinal || report.ResidualReferenceRevision != 19 || len(report.Fills) != 1 || !report.Fills[0].Fee.Equal(d.MustDecimal(".01")) {
		t.Fatal("financial report semantics were lost")
	}
	// The DTO owns cursor values independently of the mutable decoded snapshot.
	p.CurrentDurableCursor.RawReceiptIndex++
	if p.CurrentDurableCursor.RawReceiptIndex == s.SourceCursor.RawReceiptIndex {
		t.Fatal("projection aliases source cursor")
	}
}

func TestSessionSourceProvenanceLegacyOmissionAndCuratedFields(t *testing.T) {
	legacy := sessionRecord(t, "legacy", 8, 8, "completed", "buy", "100", "101", nil)
	report, err := buildSessionReport(keyA, "legacy", legacy)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if report.SourceProvenance != nil || bytes.Contains(b, []byte(`"source_provenance"`)) {
		t.Fatal("legacy source binding was inferred")
	}
	s, _ := provenanceFixture(t)
	s.Reason = "secret raw /operator/capture.jsonl"
	s.LastInputID = "private-snapshot-command"
	s.LastInputDigest = "private-runtime-digest"
	report, err = buildSessionReport(keyA, s.Parent.ID, provenanceSaved(t, s))
	if err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret raw", "/operator/capture.jsonl", "private-snapshot-command", "private-runtime-digest", "source_receipt_digest", "pending_facts", "last_input_id", "raw.jsonl", "source-manifest.json"} {
		if bytes.Contains(b, []byte(forbidden)) {
			t.Fatalf("uncurated field exposed: %s", forbidden)
		}
	}
	var object map[string]json.RawMessage
	_ = json.Unmarshal(b, &object)
	var sourceObject map[string]json.RawMessage
	_ = json.Unmarshal(object["source_provenance"], &sourceObject)
	if len(sourceObject) != 5 {
		t.Fatalf("unexpected raw provenance fields: %v", sourceObject)
	}
	if _, ok := sourceObject["sealed_source"]; !ok {
		t.Fatal("curated descriptor absent")
	}
}

func TestSessionSourceProvenanceRejectsCorruptBoundMetadata(t *testing.T) {
	state, events := provenanceFixture(t)
	for name, mutate := range map[string]func(*rt.State){
		"descriptor hash": func(s *rt.State) { s.SealedSource.Raw.SHA256 = "invalid" },
		"coherent descriptor inconsistent id": func(s *rt.State) {
			s.SealedSource.Raw.SHA256 = strings.Repeat("a", 64)
			s.Parent.SealedSource.Raw.SHA256 = s.SealedSource.Raw.SHA256
		},
		"parent descriptor mismatch": func(s *rt.State) { s.Parent.SealedSource.Normalized.SHA256 = strings.Repeat("b", 64) },
		"missing start":              func(s *rt.State) { s.Parent.SourceStartCursor = nil },
		"missing current":            func(s *rt.State) { s.SourceCursor = nil },
		"wrong source symbol":        func(s *rt.State) { s.SealedSource.Symbol = "ETH/USD"; s.Parent.SealedSource.Symbol = "ETH/USD" },
		"wrong plan symbol":          func(s *rt.State) { s.Parent.Spec.Symbol = "ETH/USD" },
		"future last decision":       func(s *rt.State) { s.LastDecisionSourceCursor = provenanceCursor(s.SealedSource, events[7]) },
		"native elapsed mismatch":    func(s *rt.State) { s.SourceCursor.NativeElapsedNS++ },
		"raw cursor regression":      func(s *rt.State) { s.LastDecisionSourceCursor.RawReceiptIndex = s.SourceCursor.RawReceiptIndex + 1 },
		"same cursor index different time": func(s *rt.State) {
			s.LastDecisionSourceCursor = provenanceCursor(s.SealedSource, events[5])
			s.LastDecisionSourceCursor.NormalizedTimeNS--
			s.LastDecisionSourceCursor.NativeElapsedNS--
		},
		"policy wrong session": func(s *rt.State) { s.LastPolicyDecision.RequestedSourceCursor.NativeSessionID = "other" },
		"policy wrong parent":  func(s *rt.State) { s.LastPolicyDecision.ParentID = "another-plan" },
		"policy application before request": func(s *rt.State) {
			s.LastPolicyDecision.RequestedSourceCursor = provenanceCursor(s.SealedSource, events[4])
		},
		"policy future applied": func(s *rt.State) {
			s.LastPolicyDecision.AppliedSourceCursor = provenanceCursor(s.SealedSource, events[7])
		},
		"unbound descriptor with cursors": func(s *rt.State) { s.SealedSource = nil },
	} {
		t.Run(name, func(t *testing.T) {
			copy, err := rt.DecodeSnapshot(provenanceSaved(t, state).Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&copy)
			if _, err := buildSessionReport(keyA, copy.Parent.ID, provenanceSaved(t, copy)); !errors.Is(err, rt.ErrInvalid) {
				t.Fatalf("corrupt source metadata accepted: %v", err)
			}
		})
	}
}

func TestSessionSourceProvenancePolicyPendingAndFrozenTerminalConsistency(t *testing.T) {
	s, events := provenanceFixture(t)
	s.PolicyPending = &rt.PolicyDecision{ParentID: s.Parent.ID, RequestedSourceCursor: provenanceCursor(s.SealedSource, events[5])}
	out, err := sourceProvenance(s)
	if err != nil || out.LatestPolicy == nil || out.LatestPolicy.AppliedCursor != nil || !reflect.DeepEqual(out.LatestPolicy.RequestedCursor, cursorProjection(s.SourceCursor)) {
		t.Fatalf("pending cursor projection mixed different decisions: %v", err)
	}
	s.PolicyPending = nil
	reference, err := rt.DecodeSnapshot(provenanceSaved(t, s).Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	reference.SourceCursor = provenanceCursor(reference.SealedSource, events[4])
	saved := provenanceSaved(t, s)
	saved.TerminalRevision = 12
	saved.TerminalSnapshot = provenanceSaved(t, reference).Snapshot
	report, err := buildSessionReport(keyA, s.Parent.ID, saved)
	if err != nil || report.ResidualReferenceRevision != 12 || report.SourceProvenance.CurrentDurableCursor.NormalizedReceiptIndex != 5 {
		t.Fatal("terminal reference changed current durable provenance", err)
	}
	reference.Parent.SourceStartCursor = provenanceCursor(reference.SealedSource, events[1])
	saved.TerminalSnapshot = provenanceSaved(t, reference).Snapshot
	if _, err := buildSessionReport(keyA, s.Parent.ID, saved); !errors.Is(err, rt.ErrInvalid) {
		t.Fatal("terminal reference changed frozen start cursor")
	}
	legacy := sessionRecord(t, s.Parent.ID, 12, 12, "completed", "buy", "100", "110", s.Fills)
	saved.TerminalSnapshot = legacy.Snapshot
	if _, err := buildSessionReport(keyA, s.Parent.ID, saved); !errors.Is(err, rt.ErrInvalid) {
		t.Fatal("bound current report used unbound terminal reference")
	}
}
