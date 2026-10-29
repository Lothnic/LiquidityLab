package kraken

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

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

func TestChecksumCannotAliasThroughUint32Overflow(t *testing.T) {
	for _, quoted := range []bool{false, true} {
		control, _ := NewBook(10)
		valid := published()
		if quoted {
			valid.Checksum = []byte(strconv.Quote("3310070434"))
		}
		if err := control.Apply(valid, true); err != nil || !control.Valid {
			t.Fatal("valid integer CRC32 spelling rejected")
		}
		for _, cycles := range []uint64{1, 2, 1 << 30} {
			b, _ := NewBook(10)
			p := published()
			// These integers have the same low 32 bits as the valid checksum,
			// but are not CRC32 values. A narrowing conversion must not admit them.
			text := strconv.FormatUint(uint64(3310070434)+cycles*(1<<32), 10)
			if quoted {
				text = strconv.Quote(text)
			}
			p.Checksum = []byte(text)
			if err := b.Apply(p, true); err == nil || b.Valid {
				t.Fatalf("overflow checksum admitted (quoted=%t cycles=%d)", quoted, cycles)
			}
			if err := b.Apply(published(), true); err != nil || !b.Valid {
				t.Fatal("fresh valid snapshot did not recover after invalid checksum")
			}
		}
	}
}

func TestSharedCRC32WireCases(t *testing.T) {
	raw, err := os.ReadFile("../../../conformance/kraken-checksums.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Schema string
		Book   Payload
		Cases  []struct {
			Name         string
			ChecksumJSON string `json:"checksum_json"`
			Valid        bool
		}
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Schema != "kraken-crc32-v1" || len(fixture.Cases) == 0 {
		t.Fatal("invalid shared checksum fixture")
	}
	seen := map[string]bool{}
	for _, tc := range fixture.Cases {
		if tc.Name == "" || seen[tc.Name] {
			t.Fatal("missing/duplicate checksum case")
		}
		seen[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			b, _ := NewBook(10)
			payload := fixture.Book
			payload.Checksum = []byte(tc.ChecksumJSON)
			err := b.Apply(payload, true)
			if (err == nil) != tc.Valid || b.Valid != tc.Valid {
				t.Fatalf("checksum accepted=%v valid=%v want=%v", err == nil, b.Valid, tc.Valid)
			}
		})
	}
}
