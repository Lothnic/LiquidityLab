// Package domain defines the exact financial and versioned execution contracts.
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// Monetary input is limited to 128 coefficient digits and scale 36. Internal
// arithmetic permits 512 digits and scale 144; division explicitly rounds down
// to 72 places unless a caller requests another scale. No float enters money.
const MaxInputDigits = 128
const MaxInputScale = 36
const DivisionScale = 72
const MaxInternalDigits = 512
const MaxInternalScale = 144

type Decimal struct {
	coefficient *big.Int
	scale       int32
}
type Rounding uint8

const (
	TowardZero Rounding = iota
	Floor
	Ceiling
	HalfEven
)

var Zero Decimal
var One = MustDecimal("1")

// All callers treat these private factors as immutable operands. The largest
// exponent used by division is internal scale + division output scale.
var decimalPowers = func() [MaxInternalScale + DivisionScale + 1]*big.Int {
	var values [MaxInternalScale + DivisionScale + 1]*big.Int
	values[0] = big.NewInt(1)
	for i := 1; i < len(values); i++ {
		values[i] = new(big.Int).Mul(values[i-1], big.NewInt(10))
	}
	return values
}()

func pow10(n int32) *big.Int {
	if n < 0 || int(n) >= len(decimalPowers) {
		panic("unsupported decimal exponent")
	}
	return decimalPowers[n]
}
func (d Decimal) coef() *big.Int {
	if d.coefficient == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(d.coefficient)
}
func makeDecimal(n *big.Int, s int32) Decimal {
	if s < 0 || s > MaxInternalScale {
		panic("decimal arithmetic precision envelope exceeded")
	}
	if n.Sign() == 0 {
		return Zero
	}
	text := n.String()
	if len(strings.TrimPrefix(text, "-")) > MaxInternalDigits {
		panic("decimal arithmetic precision envelope exceeded")
	}
	// Exact normalization uses one division instead of allocating one pair of
	// big integers for each removable fractional zero (often 72 per feature).
	trim := min(int(s), len(text)-len(strings.TrimRight(text, "0")))
	if trim > 0 {
		n = new(big.Int).Quo(n, pow10(int32(trim)))
		s -= int32(trim)
	} else {
		n = new(big.Int).Set(n)
	}
	return Decimal{n, s}
}

// ParseInternalDecimal roundtrips derived values from trusted persisted state.
// It accepts plain decimal text only and must not replace external input limits.
func ParseInternalDecimal(s string) (Decimal, error) {
	if len(s) == 0 || len(s) > MaxInternalDigits+MaxInternalScale+3 {
		return Zero, errors.New("invalid internal decimal length")
	}
	negative := strings.HasPrefix(s, "-")
	if negative {
		s = s[1:]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return Zero, errors.New("invalid internal decimal")
	}
	coefficient, scale := parts[0], 0
	if len(parts) == 2 {
		coefficient += parts[1]
		scale = len(parts[1])
	}
	if coefficient == "" || scale > MaxInternalScale {
		return Zero, errors.New("invalid internal decimal scale")
	}
	for _, digit := range coefficient {
		if digit < '0' || digit > '9' {
			return Zero, errors.New("invalid internal decimal coefficient")
		}
	}
	coefficient = strings.TrimLeft(coefficient, "0")
	if len(coefficient) > MaxInternalDigits {
		return Zero, errors.New("internal decimal exceeds coefficient envelope")
	}
	if coefficient == "" {
		return Zero, nil
	}
	if negative {
		coefficient = "-" + coefficient
	}
	n, ok := new(big.Int).SetString(coefficient, 10)
	if !ok {
		return Zero, errors.New("invalid internal decimal")
	}
	return makeDecimal(n, int32(scale)), nil
}
func FromCoefficient(coefficient string, scale int32) (Decimal, error) {
	if scale < 0 || scale > MaxInputScale {
		return Zero, errors.New("unsupported decimal scale")
	}
	digits := strings.TrimPrefix(coefficient, "-")
	if digits == "" || len(digits) > MaxInputDigits {
		return Zero, errors.New("unsupported decimal coefficient")
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return Zero, errors.New("invalid decimal coefficient")
		}
	}
	n, ok := new(big.Int).SetString(coefficient, 10)
	if !ok {
		return Zero, errors.New("invalid decimal coefficient")
	}
	return makeDecimal(n, scale), nil
}
func ParseDecimal(s string) (Decimal, error) {
	if s == "" || len(s) > MaxInputDigits+MaxInputScale+16 {
		return Zero, errors.New("invalid decimal length")
	}
	parts := strings.Split(strings.ToLower(s), "e")
	if len(parts) > 2 {
		return Zero, errors.New("invalid decimal")
	}
	exp := int64(0)
	if len(parts) == 2 {
		v, e := strconv.ParseInt(parts[1], 10, 32)
		if e != nil || v > 128 || v < -36 {
			return Zero, errors.New("invalid decimal exponent")
		}
		exp = v
	}
	mantissa := parts[0]
	if strings.HasPrefix(mantissa, "+") {
		mantissa = mantissa[1:]
	}
	dot := strings.Split(mantissa, ".")
	if len(dot) > 2 {
		return Zero, errors.New("invalid decimal")
	}
	scale := int64(0)
	coeff := dot[0]
	if len(dot) == 2 {
		scale = int64(len(dot[1]))
		coeff += dot[1]
	}
	scale -= exp
	if scale < 0 {
		coeff += strings.Repeat("0", int(-scale))
		scale = 0
	}
	if scale > 36 {
		return Zero, errors.New("unsupported decimal scale")
	}
	return FromCoefficient(coeff, int32(scale))
}
func MustDecimal(s string) Decimal {
	d, e := ParseDecimal(s)
	if e != nil {
		panic(e)
	}
	return d
}
func (d Decimal) Coefficient() string { return d.coef().String() }
func (d Decimal) Scale() int32        { return d.scale }
func (d Decimal) String() string {
	n := d.coef()
	neg := n.Sign() < 0
	n.Abs(n)
	s := n.String()
	if d.scale > 0 {
		if len(s) <= int(d.scale) {
			s = strings.Repeat("0", int(d.scale)-len(s)+1) + s
		}
		i := len(s) - int(d.scale)
		s = s[:i] + "." + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}
func (d Decimal) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }
func (d *Decimal) UnmarshalJSON(b []byte) error {
	var s string
	if e := json.Unmarshal(b, &s); e != nil {
		return errors.New("decimal must be a JSON string")
	}
	v, e := ParseDecimal(s)
	if e == nil {
		*d = v
	}
	return e
}
func align(a, b Decimal) (*big.Int, *big.Int, int32) {
	s := a.scale
	if b.scale > s {
		s = b.scale
	}
	return new(big.Int).Mul(a.coef(), pow10(s-a.scale)), new(big.Int).Mul(b.coef(), pow10(s-b.scale)), s
}
func (d Decimal) Add(b Decimal) Decimal { a, c, s := align(d, b); return makeDecimal(a.Add(a, c), s) }
func (d Decimal) Sub(b Decimal) Decimal { return d.Add(b.Neg()) }
func (d Decimal) Neg() Decimal          { n := d.coef(); return makeDecimal(n.Neg(n), d.scale) }
func (d Decimal) Mul(b Decimal) Decimal {
	return makeDecimal(new(big.Int).Mul(d.coef(), b.coef()), d.scale+b.scale)
}
func (d Decimal) Cmp(b Decimal) int {
	if d.scale == b.scale {
		if d.coefficient == nil {
			return -b.Sign()
		}
		if b.coefficient == nil {
			return d.Sign()
		}
		return d.coefficient.Cmp(b.coefficient)
	}
	a, c, _ := align(d, b)
	return a.Cmp(c)
}
func (d Decimal) IsZero() bool { return d.Sign() == 0 }
func (d Decimal) Sign() int {
	if d.coefficient == nil {
		return 0
	}
	return d.coefficient.Sign()
}
func (d Decimal) Equal(b Decimal) bool { return d.Cmp(b) == 0 }
func (d Decimal) Div(b Decimal, scale int32, mode Rounding) Decimal {
	if b.IsZero() {
		panic("decimal division by zero")
	}
	if scale < 0 || scale > 72 {
		panic("unsupported division scale")
	}
	n := new(big.Int).Mul(d.coef(), pow10(b.scale+scale))
	den := new(big.Int).Mul(b.coef(), pow10(d.scale))
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n, den, r)
	if r.Sign() != 0 {
		sign := n.Sign() * den.Sign()
		inc := mode == Floor && sign < 0 || mode == Ceiling && sign > 0
		if mode == HalfEven {
			cmp := new(big.Int).Mul(new(big.Int).Abs(r), big.NewInt(2)).Cmp(new(big.Int).Abs(den))
			inc = cmp > 0 || cmp == 0 && q.Bit(0) == 1
		}
		if inc {
			q.Add(q, big.NewInt(int64(sign)))
		}
	}
	return makeDecimal(q, scale)
}
func (d Decimal) Quo(b Decimal) Decimal { return d.Div(b, DivisionScale, TowardZero) }
func (d Decimal) FloorStep(step Decimal) Decimal {
	if step.Sign() <= 0 {
		panic("nonpositive step")
	}
	return d.Div(step, 0, Floor).Mul(step)
}
func (d Decimal) CeilStep(step Decimal) Decimal {
	if step.Sign() <= 0 {
		panic("nonpositive step")
	}
	return d.Div(step, 0, Ceiling).Mul(step)
}
func (d Decimal) Float64() float64 {
	f, _ := new(big.Rat).SetFrac(d.coef(), pow10(d.scale)).Float64()
	return f
}
func Min(ds ...Decimal) Decimal {
	d := ds[0]
	for _, v := range ds[1:] {
		if v.Cmp(d) < 0 {
			d = v
		}
	}
	return d
}
func Max(ds ...Decimal) Decimal {
	d := ds[0]
	for _, v := range ds[1:] {
		if v.Cmp(d) > 0 {
			d = v
		}
	}
	return d
}

// SecondsToNanos imports written decimal seconds with nearest, ties-to-even rounding.
// Time has its own precision envelope: 128 coefficient digits and a normalized
// decimal exponent in [-128, 128], before rounding to signed nanoseconds.
func SecondsToNanos(s string) (int64, error) {
	invalid := func() (int64, error) { return 0, fmt.Errorf("invalid replay time") }
	mantissa, exponent := s, int64(0)
	if index := strings.IndexAny(s, "eE"); index >= 0 {
		mantissa = s[:index]
		var err error
		exponent, err = strconv.ParseInt(s[index+1:], 10, 32)
		if err != nil {
			return invalid()
		}
	}
	negative := strings.HasPrefix(mantissa, "-")
	if negative || strings.HasPrefix(mantissa, "+") {
		mantissa = mantissa[1:]
	}
	var coefficient strings.Builder
	coefficient.Grow(128)
	seenDigit, seenPoint, nonzero := false, false, false
	for _, digit := range mantissa {
		if digit == '.' && !seenPoint {
			seenPoint = true
			continue
		}
		if digit < '0' || digit > '9' {
			return invalid()
		}
		seenDigit = true
		if seenPoint {
			exponent--
		}
		if digit != '0' {
			nonzero = true
		}
		if nonzero {
			if coefficient.Len() == 128 {
				return invalid()
			}
			coefficient.WriteByte(byte(digit))
		}
	}
	if !seenDigit || exponent < -128 || exponent > 128 || negative && nonzero {
		return invalid()
	}
	if !nonzero {
		return 0, nil
	}
	// Bounds are checked before constructing powers or arbitrary precision math.
	n, ok := new(big.Int).SetString(coefficient.String(), 10)
	if !ok {
		return invalid()
	}
	power := exponent + 9
	maxNanos := big.NewInt(1<<63 - 1)
	if power >= 0 {
		n.Mul(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(power), nil))
	} else {
		divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(-power), nil)
		// Reject values beyond the exact maximum, even if rounding would fit.
		if n.Cmp(new(big.Int).Mul(maxNanos, divisor)) > 0 {
			return invalid()
		}
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(n, divisor, remainder)
		tie := new(big.Int).Lsh(remainder, 1).Cmp(divisor)
		if tie > 0 || tie == 0 && quotient.Bit(0) != 0 {
			quotient.Add(quotient, big.NewInt(1))
		}
		n = quotient
	}
	if !n.IsInt64() || n.Sign() < 0 {
		return invalid()
	}
	return n.Int64(), nil
}
