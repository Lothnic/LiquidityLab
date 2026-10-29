package marketarchive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/marketdata"
	"github.com/liquiditylab/liquiditylab/internal/venue/kraken"
)

func required(b []byte, fields ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(b, &object); err != nil {
		return err
	}
	for _, key := range fields {
		value, ok := object[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("missing %s", key)
		}
	}
	return nil
}

func verify(ctx context.Context, raw, source []byte) (Recording, []byte, error) {
	fail := func(err error) (Recording, []byte, error) { return Recording{}, nil, err }
	if len(raw) == 0 || int64(len(raw)) > MaxRawBytes || int64(len(source)) > MaxManifestBytes {
		return fail(errors.New("source byte limit exceeded or empty raw capture"))
	}
	var capture marketdata.CaptureManifest
	if err := decode(source, &capture); err != nil {
		return fail(fmt.Errorf("source manifest: %w", err))
	}
	if err := required(source, "schema", "session_id", "venue", "endpoint", "symbol", "depth", "sha256", "bytes", "status", "frames", "book_messages", "valid_snapshots", "gaps", "book_valid_at_end", "max_frames", "connections"); err != nil {
		return fail(err)
	}
	if capture.Schema != marketdata.KrakenSchema || capture.Venue != "kraken" || capture.Endpoint != marketdata.KrakenPublicURL || len(capture.SessionID) == 0 || len(capture.SessionID) > 256 || strings.ContainsAny(capture.SessionID, "\r\n\x00") || !pair(capture.Symbol) {
		return fail(errors.New("invalid capture source identity"))
	}
	if _, err := kraken.NewBook(capture.Depth); err != nil {
		return fail(err)
	}
	switch capture.Status {
	case "frame_limit_reached", "duration_complete", "recorded_with_gaps":
	default:
		return fail(errors.New("capture manifest is not complete and sealed"))
	}
	if capture.Error != "" || capture.MaxFrames == 0 || capture.MaxFrames > 1_000_000 || capture.Frames > capture.MaxFrames || capture.Connections == 0 {
		return fail(errors.New("invalid capture manifest counters or error"))
	}
	if capture.Status == "frame_limit_reached" && capture.Frames != capture.MaxFrames {
		return fail(errors.New("frame limit status disagrees with counters"))
	}
	if capture.Status == "duration_complete" && capture.Frames >= capture.MaxFrames {
		return fail(errors.New("duration status disagrees with frame limit"))
	}
	started, err := time.Parse(time.RFC3339Nano, capture.StartedAt)
	if err != nil {
		return fail(errors.New("invalid capture start time"))
	}
	finished, err := time.Parse(time.RFC3339Nano, capture.FinishedAt)
	if err != nil || finished.Before(started) {
		return fail(errors.New("invalid capture finish time"))
	}
	duration, err := elapsedNS(capture.DurationSeconds)
	if err != nil || duration <= 0 {
		return fail(errors.New("invalid positive capture duration"))
	}
	requestedDuration, err := elapsedNS(capture.RequestedDurationSeconds)
	if err != nil || requestedDuration <= 0 || requestedDuration > int64(24*time.Hour) {
		return fail(errors.New("invalid positive requested capture duration"))
	}
	rawFP, sourceFP := fingerprint(raw), fingerprint(source)
	if !ValidID(capture.SHA256) || rawFP.SHA256 != capture.SHA256 || rawFP.Bytes != capture.Bytes {
		return fail(errors.New("capture raw SHA256 or byte count mismatch"))
	}
	m := Manifest{
		Schema: Schema, ID: contentID(rawFP, sourceFP), NormalizationVersion: NormalizationVersion,
		Source: Source{capture.Schema, capture.SessionID, capture.Venue, capture.Endpoint, capture.Symbol, capture.Depth},
		Raw:    rawFP, SourceManifest: sourceFP, CaptureStatus: capture.Status, Gaps: []Gap{},
		DeclaredStartedAt: started.UTC().Format(time.RFC3339Nano), DeclaredFinishedAt: finished.UTC().Format(time.RFC3339Nano),
	}
	normalizer := marketdata.NewNormalizer()
	var canonical bytes.Buffer
	var events []marketdata.Event
	var sourceGaps, nonTextFrames uint64
	lastElapsed := int64(-1)
	haveSnapshot, ended, valid, requireGap := false, false, false, false
	// A final newline seals the last JSONL receipt; an unterminated last line
	// can result from a torn write even if its partial bytes happen to parse.
	if raw[len(raw)-1] != '\n' {
		return fail(errors.New("raw capture has an unterminated final receipt"))
	}
	for offset := 0; offset < len(raw); {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		relative := bytes.IndexByte(raw[offset:], '\n')
		if relative < 0 || relative > MaxLineBytes {
			return fail(errors.New("raw receipt line limit exceeded"))
		}
		line := raw[offset : offset+relative]
		offset += relative + 1
		if len(bytes.TrimSpace(line)) == 0 {
			return fail(errors.New("blank raw receipt"))
		}
		if ended {
			return fail(errors.New("receipt after capture_end"))
		}
		var row marketdata.Receipt
		if err := decode(line, &row); err != nil {
			return fail(fmt.Errorf("receipt %d: %w", m.Receipts, err))
		}
		if err := required(line, "schema", "session_id", "depth", "receipt_index", "elapsed", "valid"); err != nil {
			return fail(err)
		}
		if requireGap && row.Kind != "gap" {
			return fail(errors.New("rejected subscription requires an explicit source gap"))
		}
		requireGap = false
		if row.Schema != capture.Schema || row.SessionID != capture.SessionID || row.Depth != capture.Depth || row.ReceiptIndex != m.Receipts {
			return fail(errors.New("receipt source identity or contiguous index mismatch"))
		}
		t, err := elapsedNS(row.Elapsed)
		if err != nil || t < lastElapsed {
			return fail(errors.New("receipt elapsed time is invalid or decreases"))
		}
		lastElapsed = t
		m.Receipts++
		channel := ""
		dataCount := 0
		subscriptionFailed := false
		switch row.Kind {
		case "", "raw":
			if row.Raw == "" || row.Reason != "" {
				return fail(errors.New("raw receipt requires a frame and no gap reason"))
			}
			m.RawFrames++
			channel, dataCount, subscriptionFailed, err = frameIdentity(row.Raw, capture.Symbol, capture.Depth)
			if err != nil {
				return fail(err)
			}
			// Bound candidate fan-out before Accept allocates its output slice.
			if (channel == "book" || channel == "trade") && dataCount > MaxEvents-len(events) {
				return fail(errors.New("normalized event count limit exceeded"))
			}
		case "gap":
			if row.Raw != "" || row.Valid || row.Reason == "" {
				return fail(errors.New("invalid source gap receipt"))
			}
			sourceGaps++
			if row.Reason == "unsupported_non_text_frame" {
				nonTextFrames++
			}
			m.Gaps = append(m.Gaps, Gap{row.ReceiptIndex, t, row.Reason})
			valid = false
		case "capture_end":
			if row.Raw != "" || row.Reason != "" || row.Valid != valid || row.Valid != capture.BookValidAtEnd {
				return fail(errors.New("capture_end disagrees with reconstructed terminal book"))
			}
			ended = true
			m.CaptureEndElapsedNS = t
			m.TerminalValid = valid
		default:
			return fail(errors.New("unsupported receipt kind"))
		}
		normalized, err := normalizer.Accept(row)
		if err != nil {
			return fail(fmt.Errorf("receipt %d normalization: %w", row.ReceiptIndex, err))
		}
		bookEvents := 0
		for _, event := range normalized {
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			if event.ReceiptSessionID != capture.SessionID || event.SourceReceiptIndex != row.ReceiptIndex || event.Symbol != capture.Symbol {
				return fail(errors.New("normalized event source mismatch"))
			}
			switch event.Kind {
			case "snapshot":
				if !haveSnapshot {
					m.FirstSnapshotElapsedNS = t
					m.FirstSnapshotSourceReceiptIndex = row.ReceiptIndex
					haveSnapshot = true
				}
				m.Snapshots++
				m.BookMessages++
				bookEvents++
				valid = true
			case "book":
				m.BookMessages++
				bookEvents++
				valid = true
			case "trade":
				m.Trades++
			case "gap":
				valid = false
				if row.Kind != "gap" {
					m.Gaps = append(m.Gaps, Gap{row.ReceiptIndex, t, event.Reason})
				}
			default:
				return fail(errors.New("unexpected normalized event kind"))
			}
			if len(events) >= MaxEvents {
				return fail(errors.New("normalized event count limit exceeded"))
			}
			encoded, err := json.Marshal(event)
			if err != nil {
				return fail(err)
			}
			if canonical.Len()+len(encoded)+1 > MaxNormalizedBytes {
				return fail(errors.New("normalized byte limit exceeded"))
			}
			canonical.Write(encoded)
			canonical.WriteByte('\n')
			events = append(events, event)
		}
		if subscriptionFailed {
			valid = false
			requireGap = true
		}
		if channel == "book" && bookEvents == 0 && len(normalized) == 0 {
			// Normalizer intentionally suppresses all pre-origin events. Preserve
			// the evidence that an early book frame could not establish a book.
			valid = false
			m.Gaps = append(m.Gaps, Gap{row.ReceiptIndex, t, "book frame before first verified snapshot was not verified"})
		}
		if (row.Kind == "" || row.Kind == "raw") && row.Valid != valid {
			return fail(errors.New("raw valid flag disagrees with checksum reconstructed book"))
		}
	}
	if !ended || !haveSnapshot {
		return fail(errors.New("capture requires a final capture_end and a verified snapshot"))
	}
	if duration < m.CaptureEndElapsedNS {
		return fail(errors.New("capture duration precedes capture_end"))
	}
	if capture.Frames != m.RawFrames+nonTextFrames || capture.BookMessages != m.BookMessages || capture.ValidSnapshots != m.Snapshots || capture.Gaps != sourceGaps {
		return fail(errors.New("capture counters disagree with reconstructed receipts"))
	}
	if (sourceGaps > 0) != (capture.Status == "recorded_with_gaps") {
		return fail(errors.New("capture gap status disagrees with source gaps"))
	}
	m.EventCount = uint64(len(events))
	m.CoverageEndNS = m.CaptureEndElapsedNS - m.FirstSnapshotElapsedNS
	m.Continuous = len(m.Gaps) == 0 && m.TerminalValid && capture.Status != "recorded_with_gaps"
	m.Normalized = fingerprint(canonical.Bytes())
	encoded, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fail(err)
	}
	if len(encoded)+1 > int(MaxManifestBytes) {
		return fail(errors.New("archive manifest byte limit exceeded"))
	}
	return Recording{m, events}, canonical.Bytes(), nil
}

func elapsedNS(text string) (int64, error) {
	if len(text) == 0 || len(text) > 128 {
		return 0, errors.New("invalid bounded elapsed time")
	}
	n, err := domain.SecondsToNanos(text)
	if err != nil || n < 0 {
		return 0, errors.New("invalid nonnegative elapsed time")
	}
	return n, nil
}

// Inspect symbol declarations even in frames which Normalizer suppresses
// before the first snapshot or ignores as trade snapshots/control messages.
func frameIdentity(raw, symbol string, depth int) (string, int, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return "", 0, false, errors.New("raw venue frame must be a JSON object")
	}
	var msg struct {
		Channel string `json:"channel"`
		Type    string `json:"type"`
		Method  string `json:"method"`
		Success *bool  `json:"success"`
		Data    []struct {
			Symbol string `json:"symbol"`
		} `json:"data"`
		Result struct {
			Symbol string `json:"symbol"`
			Depth  int    `json:"depth"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		return "", 0, false, err
	}
	if msg.Result.Symbol != "" && msg.Result.Symbol != symbol || msg.Result.Depth != 0 && msg.Result.Depth != depth {
		return "", 0, false, errors.New("subscription source metadata mismatch")
	}
	if msg.Channel == "book" || msg.Channel == "trade" {
		if len(msg.Data) == 0 || (msg.Type != "snapshot" && msg.Type != "update") {
			return "", 0, false, errors.New("invalid market frame envelope")
		}
		for _, item := range msg.Data {
			if item.Symbol != symbol {
				return "", 0, false, errors.New("market frame symbol changed")
			}
		}
	}
	return msg.Channel, len(msg.Data), msg.Method == "subscribe" && msg.Success != nil && !*msg.Success, nil
}
