// Package marketdata contains transport-level public venue recording helpers.
package marketdata

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/liquiditylab/liquiditylab/internal/domain"
	"strconv"
	"strings"

	"github.com/liquiditylab/liquiditylab/internal/venue/kraken"
)

const KrakenSchema = "liquiditylab.kraken.raw.v1"

const (
	maxDecimalText   = 128
	maxDecimalDigits = 64
	maxExponent      = 32
)

type Receipt struct {
	Schema       string `json:"schema"`
	SessionID    string `json:"session_id"`
	Depth        int    `json:"depth"`
	ReceiptIndex uint64 `json:"receipt_index"`
	Elapsed      string `json:"elapsed"`
	ReceivedAt   string `json:"received_at,omitempty"`
	Raw          string `json:"raw,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Valid        bool   `json:"valid"`
}

// Recorder serializes each source message unchanged, keeping wire decimals as raw text.
type Recorder struct {
	w       *bufio.Writer
	session string
	depth   int
	index   uint64
}

func NewRecorder(w *bufio.Writer, session string, depth int) *Recorder {
	return &Recorder{w: w, session: session, depth: depth}
}
func (r *Recorder) WriteRaw(raw string, elapsed string, valid bool) error {
	return r.write(Receipt{Raw: raw, Elapsed: elapsed, Valid: valid})
}
func (r *Recorder) WriteGap(reason, elapsed string) error {
	return r.write(Receipt{Kind: "gap", Reason: reason, Elapsed: elapsed})
}
func (r *Recorder) write(v Receipt) error {
	_, err := r.writeReceipt(v)
	return err
}
func (r *Recorder) writeReceipt(v Receipt) (Receipt, error) {
	v.Schema = KrakenSchema
	v.SessionID = r.session
	v.Depth = r.depth
	v.ReceiptIndex = r.index
	r.index++
	b, e := json.Marshal(v)
	if e != nil {
		return v, e
	}
	if _, e = r.w.Write(append(b, '\n')); e != nil {
		return v, e
	}
	return v, r.w.Flush()
}

type Event struct {
	ReceiptSessionID   string     `json:"receipt_session_id,omitempty"`
	Kind               string     `json:"kind"`
	Time               int64      `json:"time_ns"`
	Type               string     `json:"type"`
	ReceiptIndex       uint64     `json:"receipt_index"`
	SourceReceiptIndex uint64     `json:"source_receipt_index"`
	Symbol             string     `json:"symbol,omitempty"`
	Valid              bool       `json:"valid"`
	Reason             string     `json:"reason,omitempty"`
	Bids               [][]string `json:"bids,omitempty"`
	Asks               [][]string `json:"asks,omitempty"`
	Checksum           uint32     `json:"checksum,omitempty"`
	ExchangeTimestamp  string     `json:"exchange_timestamp,omitempty"`
	Price              string     `json:"price,omitempty"`
	Quantity           string     `json:"quantity,omitempty"`
	Side               string     `json:"side,omitempty"`
	TradeID            string     `json:"trade_id,omitempty"`
}

func ns(seconds string) (int64, error) {
	if e := checkDecimalInput(seconds); e != nil {
		return 0, errors.New("invalid elapsed time")
	}
	return domain.SecondsToNanos(seconds)
}
func wireString(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if err := checkDecimalInput(s); err != nil {
			return "", err
		}
		return fixedExponentDecimal(s), nil
	}
	if len(raw) == 0 {
		return "", errors.New("missing number")
	}
	s = string(raw)
	if err := checkDecimalInput(s); err != nil {
		return "", err
	}
	return fixedExponentDecimal(s), nil
}

// fixedExponentDecimal mirrors Python Decimal's fixed-point rendering while
// leaving the original JSON frame untouched in the raw recording.
func fixedExponentDecimal(s string) string {
	expAt := strings.IndexAny(s, "eE")
	if expAt < 0 {
		return s
	}
	mantissa, exponentText := s[:expAt], s[expAt+1:]
	exponent, _ := strconv.Atoi(exponentText)
	sign := ""
	if mantissa[0] == '-' {
		sign, mantissa = "-", mantissa[1:]
	} else if mantissa[0] == '+' {
		mantissa = mantissa[1:]
	}
	point := strings.IndexByte(mantissa, '.')
	if point < 0 {
		point = len(mantissa)
	}
	digits := strings.ReplaceAll(mantissa, ".", "")
	position := point + exponent
	if position <= 0 {
		return sign + "0." + strings.Repeat("0", -position) + digits
	}
	if position >= len(digits) {
		return sign + digits + strings.Repeat("0", position-len(digits))
	}
	return sign + digits[:position] + "." + digits[position:]
}

// checkDecimalInput caps decimal complexity before math/big can allocate from
// untrusted receipt or venue data. The original text is returned unchanged.
func checkDecimalInput(s string) error {
	if len(s) == 0 || len(s) > maxDecimalText {
		return errors.New("decimal text exceeds bounds")
	}
	i := 0
	if s[i] == '-' || s[i] == '+' {
		i++
		if i == len(s) {
			return errors.New("invalid decimal")
		}
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		digits++
		i++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			digits++
			i++
		}
	}
	if digits == 0 || digits > maxDecimalDigits {
		return errors.New("decimal digit count exceeds bounds")
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		start := i
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		digitStart := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if digitStart == i || i-digitStart > 3 {
			return errors.New("decimal exponent exceeds bounds")
		}
		exp, err := strconv.Atoi(s[start:i])
		if err != nil || exp < -maxExponent || exp > maxExponent {
			return errors.New("decimal exponent exceeds bounds")
		}
	}
	if i != len(s) {
		return errors.New("invalid decimal")
	}
	return nil
}

type wireLevel struct {
	Price json.RawMessage `json:"price"`
	Qty   json.RawMessage `json:"qty"`
}
type wireItem struct {
	Symbol    string          `json:"symbol"`
	Bids      []wireLevel     `json:"bids"`
	Asks      []wireLevel     `json:"asks"`
	Checksum  json.RawMessage `json:"checksum"`
	Timestamp string          `json:"timestamp"`
	Price     json.RawMessage `json:"price"`
	Qty       json.RawMessage `json:"qty"`
	Side      string          `json:"side"`
	TradeID   json.RawMessage `json:"trade_id"`
}
type envelope struct {
	Channel string     `json:"channel"`
	Type    string     `json:"type"`
	Data    []wireItem `json:"data"`
}

func levels(v []wireLevel) ([]kraken.Level, error) {
	out := make([]kraken.Level, 0, len(v))
	for _, x := range v {
		p, e := wireString(x.Price)
		if e != nil {
			return nil, e
		}
		q, e := wireString(x.Qty)
		if e != nil {
			return nil, e
		}
		out = append(out, kraken.Level{Price: p, Qty: q})
	}
	return out, nil
}
func scalarID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("missing trade_id")
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	return string(raw), nil
}

// Normalize validates causal receipt time and the complete checksum chain using
// the same incremental state as capture callbacks. Times are integer nanoseconds.
func Normalize(path string) ([]Event, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64*1024), 8*1024*1024)
	normalizer := NewNormalizer()
	var out []Event
	line := 0
	for scan.Scan() {
		line++
		rawline := bytes.TrimSpace(scan.Bytes())
		if len(rawline) == 0 {
			continue
		}
		var row Receipt
		if e = json.Unmarshal(rawline, &row); e != nil {
			return nil, fmt.Errorf("line %d: %w", line, e)
		}
		events, err := normalizer.Accept(row)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, events...)
	}
	if e = scan.Err(); e != nil {
		return nil, e
	}
	if !normalizer.haveOrigin {
		return nil, errors.New("recording contains no valid checksum-verified book snapshot")
	}
	return out, nil
}

func copyLevels(v [][]string) [][]string {
	o := make([][]string, len(v))
	for i, x := range v {
		o[i] = append([]string(nil), x...)
	}
	return o
}
func diff(prev, cur [][]string) [][]string {
	old := map[string]string{}
	now := map[string]string{}
	for _, x := range prev {
		old[x[0]] = x[1]
	}
	for _, x := range cur {
		now[x[0]] = x[1]
	}
	out := [][]string{}
	for _, x := range cur {
		if q, ok := old[x[0]]; !ok || q != x[1] {
			out = append(out, x)
		}
	}
	for _, x := range prev {
		if _, ok := now[x[0]]; !ok {
			out = append(out, []string{x[0], "0"})
		}
	}
	return out
}
