package kraken

import "testing"

func published() Payload {
	return Payload{Symbol: "BTC/USD", Bids: []Level{{"45283.5", "0.10000000"}, {"45283.4", "1.54582015"}, {"45282.1", "0.10000000"}, {"45281.0", "0.10000000"}, {"45280.3", "1.54592586"}, {"45279.0", "0.07990000"}, {"45277.6", "0.03310103"}, {"45277.5", "0.30000000"}, {"45277.3", "1.54602737"}, {"45276.6", "0.15445238"}}, Asks: []Level{{"45285.2", "0.00100000"}, {"45286.4", "1.54571953"}, {"45286.6", "1.54571109"}, {"45289.6", "1.54560911"}, {"45290.2", "0.15890660"}, {"45291.8", "1.54553491"}, {"45294.7", "0.04454749"}, {"45296.1", "0.35380000"}, {"45297.5", "0.09945542"}, {"45299.5", "0.18772827"}}, Checksum: []byte("3310070434")}
}
func TestPublishedChecksumAndWirePrecision(t *testing.T) {
	b, _ := NewBook(10)
	p := published()
	if err := b.Apply(p, true); err != nil {
		t.Fatal(err)
	}
	if !b.Valid || b.Checksum() != 3310070434 {
		t.Fatalf("unexpected valid/checksum: %v %d", b.Valid, b.Checksum())
	}
	if b.Levels("bids")[0][1] != "0.10000000" {
		t.Fatal("lost wire precision")
	}
}
func TestInvalidUpdateRequiresSnapshot(t *testing.T) {
	b, _ := NewBook(10)
	p := published()
	if e := b.Apply(p, true); e != nil {
		t.Fatal(e)
	}
	p.Checksum = []byte("0")
	if e := b.Apply(p, false); e == nil {
		t.Fatal("expected checksum failure")
	}
	if b.Valid {
		t.Fatal("failed update remained valid")
	}
	p = published()
	if e := b.Apply(p, false); e == nil {
		t.Fatal("accepted update after invalidation")
	}
	if e := b.Apply(p, true); e != nil {
		t.Fatal(e)
	}
}
func TestDepthValidation(t *testing.T) {
	if _, e := NewBook(11); e == nil {
		t.Fatal("accepted unsupported depth")
	}
}

func TestBookNumberBoundsBeforeRationalAllocation(t *testing.T) {
	for _, text := range []string{"1e100000000", "1/2", "0x100", "1e-100000000"} {
		if _, err := decimal(text); err == nil {
			t.Fatal("accepted unsupported decimal spelling", text)
		}
	}
}
