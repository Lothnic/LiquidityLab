package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInternalPersistenceEnvelopeAndExactNormalization(t *testing.T) {
	for _, text := range []string{strings.Repeat("9", 512), "0." + strings.Repeat("0", 143) + "1", "-123.4500"} {
		value, err := ParseInternalDecimal(text)
		if err != nil {
			t.Fatal(err)
		}
		roundtrip, err := ParseInternalDecimal(value.String())
		if err != nil || !roundtrip.Equal(value) {
			t.Fatal("internal roundtrip", text, err)
		}
	}
	for _, text := range []string{"NaN", "1e10", strings.Repeat("1", 513), "0." + strings.Repeat("0", 144) + "1"} {
		if _, err := ParseInternalDecimal(text); err == nil {
			t.Fatal("accepted unsupported internal value", text)
		}
	}
	if _, err := ParseDecimal(strings.Repeat("9", 129)); err == nil {
		t.Fatal("external envelope widened")
	}
	for i := 0; i < 100; i++ {
		if MustDecimal("1").Quo(MustDecimal("1")).String() != "1" || MustDecimal("-1").Quo(MustDecimal("2")).String() != "-0.5" {
			t.Fatal("normalization or shared factors mutated")
		}
	}
}

func TestTimeBoundsAreCheckedBeforeQuantization(t *testing.T) {
	for _, text := range []string{"-0.0000000001", "9223372036.8547758071"} {
		if _, err := SecondsToNanos(text); err == nil {
			t.Fatal("rounded unsupported source time into range", text)
		}
	}
}

func TestExactDecimalArithmeticAndImmutability(t *testing.T) {
	a := MustDecimal("1.2300")
	b := MustDecimal(".07")
	if a.Add(b).String() != "1.3" || a.Sub(b).String() != "1.16" || a.Mul(b).String() != "0.0861" || a.String() != "1.23" {
		t.Fatal("exact arithmetic or immutability")
	}
	for _, tt := range []struct {
		value       string
		floor, ceil string
	}{{"1.234", "1.23", "1.24"}, {"-1.234", "-1.24", "-1.23"}} {
		v := MustDecimal(tt.value)
		if v.FloorStep(MustDecimal(".01")).String() != tt.floor || v.CeilStep(MustDecimal(".01")).String() != tt.ceil {
			t.Fatal(tt)
		}
	}
	if got := MustDecimal("1").Div(MustDecimal("3"), 4, Floor).String(); got != "0.3333" {
		t.Fatal(got)
	}
}
func TestDecimalValidationAndJSON(t *testing.T) {
	for _, s := range []string{"NaN", "Inf", "--1", "1e999999", "1e-37", strings.Repeat("9", 129), "1.2.3", "", "."} {
		if _, err := ParseDecimal(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
	if _, err := FromCoefficient("12", 37); err == nil {
		t.Fatal("scale")
	}
	var d Decimal
	if err := json.Unmarshal([]byte("1.2"), &d); err == nil {
		t.Fatal("binary money accepted")
	}
	if err := json.Unmarshal([]byte(`"1.20"`), &d); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	if string(b) != `"1.2"` {
		t.Fatal(string(b))
	}
}
func TestTimeImportTiesToEvenAndOverflow(t *testing.T) {
	for _, tt := range []struct {
		s  string
		ns int64
	}{{"0.0000000005", 0}, {"0.0000000015", 2}, {"0.1", 100000000}, {"1.2345678906", 1234567891}} {
		v, err := SecondsToNanos(tt.s)
		if err != nil || v != tt.ns {
			t.Fatal(tt, v, err)
		}
	}
	for _, s := range []string{"-1", "9223372037"} {
		if _, err := SecondsToNanos(s); err == nil {
			t.Fatal("accepted", s)
		}
	}
}
func TestExplicitActionMapping(t *testing.T) {
	for i := 0; i < 6; i++ {
		a, err := ActionFromLegacy(i)
		if err != nil || int(a) != i+1 {
			t.Fatal(i)
		}
	}
	for _, a := range []Action{Unspecified, -1, 7} {
		if a.Validate() == nil {
			t.Fatal("unknown", a)
		}
	}
}
func FuzzDecimalRoundtrip(f *testing.F) {
	for _, s := range []string{"0", "-1.23", "1e3", ".01"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDecimal(s)
		if err != nil {
			return
		}
		again, err := ParseDecimal(d.String())
		if err != nil || !d.Equal(again) {
			t.Fatal(s, d.String(), err)
		}
	})
}
