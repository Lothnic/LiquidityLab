package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
)

func TestDecodeSnapshotPreservesInternalDecimalEnvelope(t *testing.T) {
	amount, err := d.ParseInternalDecimal("0." + strings.Repeat("123456789", 8))
	if err != nil {
		t.Fatal(err)
	}
	state := State{Version: StateVersion, Profile: ProfilePaper, Book: x.NewBook(), TradeFlow: amount}
	encoded := jsonBytes(state)
	var external State
	if err := json.Unmarshal(encoded, &external); err == nil {
		t.Fatal("ordinary external JSON decoder unexpectedly accepted internal scale")
	}
	restored, err := DecodeSnapshot(encoded)
	if err != nil || !restored.TradeFlow.Equal(amount) {
		t.Fatalf("trusted snapshot changed exact internal amount: %v", err)
	}
}

func TestDecodeSnapshotRequiresVersionedPaperProjection(t *testing.T) {
	for _, raw := range []string{`{}`, `{"version":"unknown","profile":"paper","book":{}}`, `{"version":"account-state-v1","profile":"live","book":{}}`, `{"version":"account-state-v1","profile":"paper","book":null}`} {
		if _, err := DecodeSnapshot([]byte(raw)); err == nil {
			t.Fatalf("invalid snapshot accepted: %s", raw)
		}
	}
}
