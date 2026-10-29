package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func sourceFixture(t *testing.T) (State, marketInput) {
	t.Helper()
	root := t.TempDir()
	base := "../../examples/recordings/kraken-btcusd-20261028/"
	m, err := marketarchive.Import(context.Background(), marketarchive.Config{RawPath: base + "public-capture.jsonl", ManifestPath: base + "public-capture.manifest.json", ArchiveRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	source, err := SealedSourceFromManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	s := State{Version: StateVersion, Profile: ProfilePaper, DataSource: "recorded", SealedSource: source, Rules: d.DefaultRules(), Fees: d.DefaultFees(), Execution: d.DefaultConfig(), BaseCurrency: "BTC", QuoteCurrency: "USD", InstrumentRevision: "synthetic-rules-v1", Book: x.NewBook()}
	input := marketInput{Event: d.MarketEvent{Kind: "snapshot", Symbol: source.Symbol, Bids: []d.Level{{Price: d.MustDecimal("99"), Quantity: d.One}}, Asks: []d.Level{{Price: d.MustDecimal("101"), Quantity: d.One}}}, SnapshotID: "source-event-0", SessionID: source.NativeSessionID, Source: &SourceCursor{ArchiveID: source.ArchiveID, NativeSessionID: source.NativeSessionID, RawReceiptIndex: source.FirstSnapshotRawReceiptIndex, NativeElapsedNS: source.FirstSnapshotElapsedNS}}
	return s, input
}

func TestSourceCursorOrderIdentityAndLatestRetry(t *testing.T) {
	s, input := sourceFixture(t)
	if older, err := checkSourceReceipt(s, input); err != nil || older {
		t.Fatalf("first snapshot rejected: %v", err)
	}
	acceptSourceReceipt(&s, input)
	if _, err := checkSourceReceipt(s, input); err != nil {
		t.Fatal("exact duplicate was rejected")
	}
	changed := input
	changed.Event.Price = d.MustDecimal("123")
	if _, err := checkSourceReceipt(s, changed); !errors.Is(err, pg.ErrConflict) {
		t.Fatal("same cursor with different payload accepted")
	}
	for name, alter := range map[string]func(*marketInput){
		"missing":             func(i *marketInput) { i.Source = nil },
		"archive":             func(i *marketInput) { i.Source.ArchiveID = strings.Repeat("a", 64) },
		"native session":      func(i *marketInput) { i.Source.NativeSessionID = "other" },
		"symbol":              func(i *marketInput) { i.Event.Symbol = "ETH/USD" },
		"normalized skip":     func(i *marketInput) { i.Source.NormalizedReceiptIndex = 2; i.Event.ReceiptIndex = 2 },
		"native time":         func(i *marketInput) { i.Source.NativeElapsedNS++ },
		"raw before snapshot": func(i *marketInput) { i.Source.RawReceiptIndex = 0 },
		"coverage": func(i *marketInput) {
			i.Source.NormalizedTimeNS = s.SealedSource.CoverageEndNS + 1
			i.Source.NativeElapsedNS = s.SealedSource.FirstSnapshotElapsedNS + i.Source.NormalizedTimeNS
			i.Event.Time = i.Source.NormalizedTimeNS
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := input
			candidate.Source = copySourceCursor(input.Source)
			alter(&candidate)
			if _, err := checkSourceReceipt(s, candidate); err == nil {
				t.Fatal("invalid provenance accepted")
			}
		})
	}
	second := input
	second.Source = copySourceCursor(input.Source)
	second.Source.NormalizedReceiptIndex = 1
	second.Event.ReceiptIndex = 1
	if _, err := checkSourceReceipt(s, second); err != nil {
		t.Fatalf("same raw receipt and time with next normalized index rejected: %v", err)
	}
	acceptSourceReceipt(&s, second)
	if _, err := checkSourceReceipt(s, input); err == nil {
		t.Fatal("old book accepted")
	}
	trade := input
	trade.Event.Kind = "trade"
	trade.Event.Bids = nil
	trade.Event.Asks = nil
	trade.Event.Side = "sell"
	trade.Event.Price = d.MustDecimal("99")
	trade.Event.Quantity = d.One
	if older, err := checkSourceReceipt(s, trade); err != nil || !older {
		t.Fatal("older trade did not require frozen durable proof")
	}
	if s.SourceCursor.NormalizedReceiptIndex != 1 {
		t.Fatal("validation rewound current cursor")
	}
}

func TestSourceLegacyUnboundAndBootstrapPinning(t *testing.T) {
	s, input := sourceFixture(t)
	legacy := s
	legacy.SealedSource = nil
	input.Source = nil
	if _, err := checkSourceReceipt(legacy, input); err != nil {
		t.Fatal("legacy ingress changed")
	}
	if err := validateSourceState(legacy); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(legacy)
	if !strings.Contains(string(b), `"sealed_source":null`) || !strings.Contains(string(b), `"source_cursor":null`) {
		t.Fatal("unbound state was not explicit")
	}
	cfg := Config{DataSource: s.DataSource, SealedSource: copySource(s.SealedSource), Rules: s.Rules, Fees: s.Fees, Execution: s.Execution, BaseCurrency: s.BaseCurrency, QuoteCurrency: s.QuoteCurrency, InstrumentRevision: s.InstrumentRevision}
	account := pg.Account{Snapshot: jsonBytes(s)}
	if _, err := bootstrapState(cfg, account, nil); err != nil {
		t.Fatal(err)
	}
	cfg.SealedSource = nil
	if _, err := bootstrapState(cfg, account, nil); err == nil {
		t.Fatal("bootstrap unbound a sealed account")
	}
	cfg.SealedSource = copySource(s.SealedSource)
	cfg.SealedSource.Normalized.SHA256 = strings.Repeat("a", 64)
	if _, err := bootstrapState(cfg, account, nil); err == nil {
		t.Fatal("bootstrap changed pinned normalized recording")
	}
	input.Source = &SourceCursor{ArchiveID: s.SealedSource.ArchiveID, NativeSessionID: s.SealedSource.NativeSessionID, RawReceiptIndex: s.SealedSource.FirstSnapshotRawReceiptIndex, NativeElapsedNS: s.SealedSource.FirstSnapshotElapsedNS}
	acceptSourceReceipt(&s, input)
	s.Parent = &Parent{Started: true, SealedSource: copySource(s.SealedSource), SourceStartCursor: copySourceCursor(s.SourceCursor)}
	if err := validateSourceState(s); err != nil {
		t.Fatal(err)
	}
	copy, err := cloneState(s)
	if err != nil {
		t.Fatal(err)
	}
	copy.SourceCursor.NormalizedReceiptIndex++
	if s.Parent.SourceStartCursor.NormalizedReceiptIndex != 0 || !reflect.DeepEqual(s.Parent.SealedSource, s.SealedSource) {
		t.Fatal("session start cursor or descriptor aliased mutable current cursor")
	}
	copy.Parent.SourceStartCursor.RawReceiptIndex = 0
	if err := validateSourceState(copy); err == nil {
		t.Fatal("corrupt start cursor accepted")
	}
}

func TestSourceDecisionMetadataPreservesAuthorityAndClock(t *testing.T) {
	s, input := sourceFixture(t)
	acceptSourceReceipt(&s, input)
	after, err := cloneState(s)
	if err != nil {
		t.Fatal(err)
	}
	after.LastDecisionSourceCursor = copySourceCursor(after.SourceCursor)
	after.DecisionCount++
	if !decisionBookkeepingOnly(s, after, pg.Change{}) {
		t.Fatal("source decision metadata changed financial authority")
	}
	after.Parent = &Parent{Started: true, SealedSource: copySource(s.SealedSource), SourceStartCursor: copySourceCursor(s.SourceCursor)}
	if err := validateSourceState(after); err != nil {
		t.Fatal(err)
	}
	native := after.SourceCursor.NativeElapsedNS
	accountEvent := input.Event
	accountEvent.Time = 123456789
	if after.SourceCursor.NativeElapsedNS != native || after.SourceCursor.NormalizedTimeNS != 0 || accountEvent.Time == native {
		t.Fatal("source native clock mixed with account elapsed time")
	}
	p := PolicyDecision{RequestedSourceCursor: copySourceCursor(s.SourceCursor), AppliedSourceCursor: copySourceCursor(after.SourceCursor)}
	data := jsonBytes(p)
	var decoded PolicyDecision
	if err := decodeInternal(data, &decoded); err != nil || !reflect.DeepEqual(p, decoded) {
		t.Fatal("policy cursor metadata did not round trip")
	}
}

func TestSourceOlderTradeFrozenEconomicIdentity(t *testing.T) {
	s, input := sourceFixture(t)
	input.Event.Kind = "trade"
	input.Event.Bids = nil
	input.Event.Asks = nil
	input.Event.Price = d.MustDecimal("99")
	input.Event.Quantity = d.One
	input.Event.Side = "sell"
	input.Event.TradeID = "native-trade"
	id, digest := paperTradeIdentity(input.SessionID, input.Event)
	fact := paperTradeFact{ID: id, InputDigest: digest, SourceID: input.SessionID, Event: input.Event}
	if !frozenSourceTradeMatches(s, input, fact) {
		t.Fatal("accepted frozen economic identity rejected")
	}
	changed := input
	changed.Event.Price = d.MustDecimal("98")
	if frozenSourceTradeMatches(s, changed, fact) {
		t.Fatal("conflicting older trade payload accepted")
	}
	changed = input
	changed.Event.Time = 12345
	changed.Event.ReceiptIndex = 42
	if !frozenSourceTradeMatches(s, changed, fact) {
		t.Fatal("venue identity no longer deduplicates repeated native receipts")
	}
	fact.SourceID = "different-native-session"
	if frozenSourceTradeMatches(s, input, fact) {
		t.Fatal("frozen outcome from another native source accepted")
	}
	acceptSourceReceipt(&s, input)
	s.Book.Valid = false
	if _, err := checkSourceReceipt(s, input); err != nil || !sourceNeedsFrozenTradeRetry(s, input) {
		t.Fatal("latest accepted trade retry after restart bypassed frozen obligation lookup")
	}
	s.Book.Valid = true
	if sourceNeedsFrozenTradeRetry(s, input) {
		t.Fatal("healthy latest trade retry unnecessarily bypasses normal durable deduplication")
	}
}

func TestSourceUncertainReloadQuarantinesCursorBookMismatch(t *testing.T) {
	durable, input := sourceFixture(t)
	acceptSourceReceipt(&durable, input)
	durable.Book.Bids = map[string]d.Level{"99": {Price: d.MustDecimal("99"), Quantity: d.One}}
	durable.Book.Asks = map[string]d.Level{"101": {Price: d.MustDecimal("101"), Quantity: d.One}}
	durable.Book.Valid = true
	durable.Book.Debt["bids"]["99"] = d.MustDecimal(".2")
	durable.Fills = []d.Fill{{ExecutionID: "durable-fill", Quantity: d.MustDecimal(".2")}}
	durable.Child = &Child{ID: "durable-child", State: "open"}
	prior, err := cloneState(durable)
	if err != nil {
		t.Fatal(err)
	}
	input.Source = copySourceCursor(input.Source)
	input.Source.NormalizedReceiptIndex = 1
	input.Event.ReceiptIndex = 1
	input.Event.Kind = "trade"
	acceptSourceReceipt(&prior, input)
	prior.Book.Debt["bids"]["99"] = d.MustDecimal(".9")
	prior.Fills = nil
	prior.Child = nil
	if !restoreSourceAfterReload(&durable, prior) {
		t.Fatal("lost RAM source cursor was not quarantined")
	}
	if durable.Book.Valid || !durable.RecoveryRequired || durable.Reason != "sealed_source_snapshot_required_after_reload" || durable.SourceCursor.NormalizedReceiptIndex != 1 {
		t.Fatal("newer cursor was paired with a valid older book")
	}
	if !durable.Book.Debt["bids"]["99"].Equal(d.MustDecimal(".2")) || len(durable.Fills) != 1 || durable.Child == nil || durable.Child.ID != "durable-child" {
		t.Fatal("reload replaced durable financial obligations with RAM state")
	}
	if restoreSourceAfterReload(&durable, prior) {
		t.Fatal("same current source cursor was treated as another lost book")
	}
	legacy := durable
	legacy.SealedSource = nil
	legacy.SourceCursor = nil
	if restoreSourceAfterReload(&legacy, prior) {
		t.Fatal("reload inferred a missing source binding")
	}
}
