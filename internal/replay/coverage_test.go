package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

func TestExplicitTapeQuietTailCoverage(t *testing.T) {
	events := []d.MarketEvent{snapshot(0, "99", "101")}
	legacy, err := NewTape(events)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(events)
	sum := sha256.Sum256(encoded)
	if legacy.hash != hex.EncodeToString(sum[:]) {
		t.Fatal("legacy tape hash changed")
	}
	if _, err := NewWithTape(legacy, plan(), d.DefaultRules(), d.DefaultFees(), config()); err == nil {
		t.Fatal("legacy coverage expanded")
	}
	covered, err := NewTapeWithCoverage(events, 3*d.Second)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewWithTape(covered, plan(), d.DefaultRules(), d.DefaultFees(), config())
	if err != nil {
		t.Fatal(err)
	}
	if len(e.events) != 1 || covered.hash == legacy.hash {
		t.Fatal("coverage fabricated events or omitted hash identity")
	}
	for !e.Terminated && !e.Truncated {
		if _, err := e.Step(d.Hold); err != nil {
			t.Fatal(err)
		}
	}
	if !e.Terminated || e.Truncated {
		t.Fatal("quiet tail could not terminate")
	}
	short, _ := NewTapeWithCoverage(events, 3*d.Second-1)
	if _, err := NewWithTape(short, plan(), d.DefaultRules(), d.DefaultFees(), config()); err == nil {
		t.Fatal("short coverage accepted")
	}
	if _, err := NewTapeWithCoverage([]d.MarketEvent{snapshot(0, "99", "101"), snapshot(d.Second, "99", "101")}, d.Second-1); err == nil {
		t.Fatal("coverage before last event accepted")
	}
}
