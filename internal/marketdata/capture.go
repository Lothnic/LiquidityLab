package marketdata

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mrand "math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/liquiditylab/liquiditylab/internal/venue/kraken"
)

const (
	KrakenPublicURL  = "wss://ws.kraken.com/v2"
	maxWireFrame     = 4_000_000
	maxCaptureFrames = 1_000_000
	maxCaptureTime   = 24 * time.Hour
	maxDialTime      = 10 * time.Second
)

// CaptureConfig bounds both wall time and the number of received WebSocket frames.
// A caller-supplied context can impose an earlier deadline or cancellation.
type CaptureConfig struct {
	Symbol    string
	Output    string
	Duration  time.Duration
	Depth     int
	MaxFrames uint64
	MaxBytes  int64
}

// CaptureManifest describes one immutable raw JSONL capture.
type CaptureManifest struct {
	Schema                   string `json:"schema"`
	SessionID                string `json:"session_id"`
	Venue                    string `json:"venue"`
	Endpoint                 string `json:"endpoint"`
	Symbol                   string `json:"symbol"`
	Depth                    int    `json:"depth"`
	Output                   string `json:"output"`
	StartedAt                string `json:"started_at"`
	FinishedAt               string `json:"finished_at"`
	RequestedDurationSeconds string `json:"requested_duration_seconds"`
	DurationSeconds          string `json:"duration_seconds"`
	MaxFrames                uint64 `json:"max_frames"`
	Frames                   uint64 `json:"frames"`
	BookMessages             uint64 `json:"book_messages"`
	ValidSnapshots           uint64 `json:"valid_snapshots"`
	Gaps                     uint64 `json:"gaps"`
	Connections              uint64 `json:"connections"`
	BookValidAtEnd           bool   `json:"book_valid_at_end"`
	SHA256                   string `json:"sha256"`
	Bytes                    int64  `json:"bytes"`
	Status                   string `json:"status"`
	Error                    string `json:"error,omitempty"`
}

// Capture records the unauthenticated Kraken v2 book and trade streams. Received
// text frames are persisted unchanged after structural validation. Transport failures,
// subscription rejection, and invalid book state are represented as explicit
// receipt gaps so downstream replay cannot treat the recording as continuous.
func Capture(ctx context.Context, cfg CaptureConfig) (manifest CaptureManifest, retErr error) {
	if !isPair(cfg.Symbol) {
		return manifest, errors.New("symbol must be a Kraken v2 pair such as BTC/USD")
	}
	if cfg.Duration <= 0 || !finiteDuration(cfg.Duration) || cfg.Duration > maxCaptureTime {
		return manifest, fmt.Errorf("duration must be finite and between 0 and %s", maxCaptureTime)
	}
	if cfg.MaxFrames == 0 || cfg.MaxFrames > maxCaptureFrames {
		return manifest, fmt.Errorf("max frames must be between 1 and %d", maxCaptureFrames)
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = 64 << 20
	}
	if cfg.MaxBytes < 1024 || cfg.MaxBytes > 8<<30 {
		return manifest, errors.New("max bytes must be between 1024 and 8GiB")
	}
	if _, err := kraken.NewBook(cfg.Depth); err != nil {
		return manifest, err
	}
	if cfg.Output == "" {
		return manifest, errors.New("output path is required")
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Output), 0o755); err != nil {
		return manifest, err
	}
	f, err := os.OpenFile(cfg.Output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return manifest, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()

	sessionID, err := newSessionID()
	if err != nil {
		_ = f.Close()
		return manifest, err
	}
	started := time.Now()
	manifest = CaptureManifest{
		Schema: KrakenSchema, SessionID: sessionID, Venue: "kraken",
		Endpoint: KrakenPublicURL, Symbol: cfg.Symbol, Depth: cfg.Depth,
		Output: cfg.Output, StartedAt: started.UTC().Format(time.RFC3339Nano),
		RequestedDurationSeconds: seconds(cfg.Duration), MaxFrames: cfg.MaxFrames,
		Status: "capturing",
	}
	sha := sha256.New()
	budget := &captureWriter{writer: io.MultiWriter(f, sha), limit: cfg.MaxBytes}
	stream := bufio.NewWriterSize(budget, 64*1024)
	recorder := NewRecorder(stream, sessionID, cfg.Depth)
	book, _ := kraken.NewBook(cfg.Depth)
	deadline := started.Add(cfg.Duration)
	backoffAttempt := 0
	for manifest.Frames < cfg.MaxFrames && time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			manifest.Error = err.Error()
			retErr = writeGap(recorder, f, started, "capture_cancelled: "+err.Error())
			if retErr == nil {
				retErr = err
			}
			break
		}

		dialDeadline := time.Now().Add(maxDialTime)
		if deadline.Before(dialDeadline) {
			dialDeadline = deadline
		}
		connCtx, cancel := context.WithDeadline(ctx, dialDeadline)
		conn, _, dialErr := websocket.Dial(connCtx, KrakenPublicURL, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
		if dialErr != nil {
			cancel()
			if ctx.Err() != nil {
				manifest.Error = ctx.Err().Error()
				retErr = writeGap(recorder, f, started, "capture_cancelled: "+ctx.Err().Error())
				if retErr == nil {
					retErr = ctx.Err()
				}
				break
			}
			manifest.Gaps++
			if e := writeGap(recorder, f, started, "websocket_connect: "+dialErr.Error()); e != nil {
				retErr = e
				break
			}
			if time.Now().Before(deadline) {
				backoffAttempt++
				if !pauseUntil(ctx, deadline, reconnectBackoff(backoffAttempt)) {
					continue
				}
			}
			continue
		}
		manifest.Connections++
		conn.SetReadLimit(maxWireFrame)
		for _, sub := range subscriptions(cfg.Symbol, cfg.Depth) {
			writeCtx, writeCancel := context.WithDeadline(ctx, deadline)
			writeErr := conn.Write(writeCtx, websocket.MessageText, sub)
			writeCancel()
			if writeErr != nil {
				dialErr = writeErr
				break
			}
		}

		forceReconnect := false
		for dialErr == nil && manifest.Frames < cfg.MaxFrames {
			if ctx.Err() != nil {
				dialErr = ctx.Err()
				break
			}
			readCtx, readCancel := context.WithDeadline(ctx, deadline)
			msgType, payload, readErr := conn.Read(readCtx)
			readCancel()
			if readErr != nil {
				dialErr = readErr
				break
			}
			manifest.Frames++
			if msgType != websocket.MessageText {
				book.Valid = false
				manifest.Gaps++
				if e := writeGap(recorder, f, started, "unsupported_non_text_frame"); e != nil {
					retErr = e
					break
				}
				continue
			}

			raw := string(payload)
			valid, bookCount, snapshots, validationErr := validateFrame(raw, cfg.Symbol, book)
			manifest.BookMessages += bookCount
			if validationErr != nil {
				valid = false
				book.Valid = false
				forceReconnect = true
			} else {
				manifest.ValidSnapshots += snapshots
			}
			if err := writeRaw(recorder, f, started, raw, valid); err != nil {
				retErr = err
				break
			}
			if validationErr != nil {
				manifest.Gaps++
				if err := writeGap(recorder, f, started, validationErr.Error()); err != nil {
					retErr = err
					break
				}
				break
			}
			if snapshots > 0 {
				backoffAttempt = 0
			}
		}
		_ = conn.Close(websocket.StatusNormalClosure, "capture interval ended")
		cancel()
		if retErr != nil {
			break
		}
		if ctx.Err() != nil {
			manifest.Error = ctx.Err().Error()
			retErr = writeGap(recorder, f, started, "capture_cancelled: "+ctx.Err().Error())
			if retErr == nil {
				retErr = ctx.Err()
			}
			break
		}
		if forceReconnect && time.Now().Before(deadline) && manifest.Frames < cfg.MaxFrames {
			backoffAttempt++
			if !pauseUntil(ctx, deadline, reconnectBackoff(backoffAttempt)) {
				continue
			}
			continue
		}
		if time.Now().Before(deadline) && manifest.Frames < cfg.MaxFrames {
			book.Valid = false
			manifest.Gaps++
			if err := writeGap(recorder, f, started, "connection_ended_resnapshot_required: "+dialErr.Error()); err != nil {
				retErr = err
				break
			}
			backoffAttempt++
			if !pauseUntil(ctx, deadline, reconnectBackoff(backoffAttempt)) {
				continue
			}
		}
	}

	if retErr == nil && manifest.ValidSnapshots == 0 {
		retErr = errors.New("capture ended without a valid checksum-verified book snapshot")
		manifest.Error = retErr.Error()
	}
	if retErr == nil {
		if ctx.Err() != nil {
			retErr = ctx.Err()
			manifest.Error = retErr.Error()
		} else if manifest.Frames >= cfg.MaxFrames {
			manifest.Status = "frame_limit_reached"
		} else {
			manifest.Status = "duration_complete"
		}
	}
	if manifest.ValidSnapshots == 0 {
		manifest.Status = "no_valid_book"
	} else if manifest.Error != "" || retErr != nil {
		manifest.Status = "incomplete"
	} else if manifest.Gaps > 0 {
		manifest.Status = "recorded_with_gaps"
	}
	manifest.BookValidAtEnd = book.Valid
	if err := recorder.write(Receipt{Kind: "capture_end", Elapsed: elapsed(started), Valid: book.Valid}); err != nil && retErr == nil {
		retErr = err
	}
	if err := stream.Flush(); err != nil && retErr == nil {
		retErr = err
	}
	if err := f.Sync(); err != nil && retErr == nil {
		retErr = err
	}
	manifest.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	manifest.DurationSeconds = seconds(time.Since(started))
	manifest.SHA256 = hex.EncodeToString(sha.Sum(nil))
	manifest.Bytes = budget.written
	if retErr != nil && manifest.Error == "" {
		manifest.Error = retErr.Error()
		manifest.Status = "incomplete"
	}
	if err := f.Close(); err != nil && retErr == nil {
		retErr = err
	}
	closed = true
	if err := writeManifest(cfg.Output, manifest); err != nil && retErr == nil {
		retErr = err
	}
	return manifest, retErr
}

// A rejected write never starts a record that would exceed the disk budget.
type captureWriter struct {
	writer         io.Writer
	limit, written int64
}

func (w *captureWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.limit-w.written {
		return 0, errors.New("capture disk byte limit reached")
	}
	n, err := w.writer.Write(p)
	w.written += int64(n)
	return n, err
}

func subscriptions(symbol string, depth int) [][]byte {
	book, _ := json.Marshal(map[string]any{"method": "subscribe", "params": map[string]any{"channel": "book", "symbol": []string{symbol}, "snapshot": true, "depth": depth}})
	trade, _ := json.Marshal(map[string]any{"method": "subscribe", "params": map[string]any{"channel": "trade", "symbol": []string{symbol}, "snapshot": true}})
	return [][]byte{book, trade}
}

func validateFrame(raw, symbol string, book *kraken.Book) (bool, uint64, uint64, error) {
	var msg envelope
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		return false, 0, 0, fmt.Errorf("invalid_json_frame: %w", err)
	}
	var control struct {
		Method  string `json:"method"`
		Success *bool  `json:"success"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal([]byte(raw), &control)
	if control.Method == "subscribe" && control.Success != nil && !*control.Success {
		return false, 0, 0, fmt.Errorf("subscription_rejected: %s", control.Error)
	}
	if msg.Channel != "book" {
		return book.Valid, 0, 0, nil
	}
	var count uint64
	var snapshots uint64
	for _, item := range msg.Data {
		if item.Symbol != symbol {
			continue
		}
		bids, err := levels(item.Bids)
		if err != nil {
			return false, count, snapshots, err
		}
		asks, err := levels(item.Asks)
		if err != nil {
			return false, count, snapshots, err
		}
		if err := book.Apply(kraken.Payload{Symbol: item.Symbol, Bids: bids, Asks: asks, Checksum: item.Checksum, Timestamp: item.Timestamp}, msg.Type == "snapshot"); err != nil {
			return false, count, snapshots, err
		}
		count++
		if msg.Type == "snapshot" {
			snapshots++
		}
	}
	return book.Valid, count, snapshots, nil
}

func reconnectBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := 250 * time.Millisecond
	for i := 1; i < attempt && base < 5*time.Second; i++ {
		base *= 2
	}
	if base > 5*time.Second {
		base = 5 * time.Second
	}
	return base + time.Duration(mrand.Int63n(int64(base/2)+1))
}

func writeRaw(r *Recorder, f *os.File, started time.Time, raw string, valid bool) error {
	err := r.write(Receipt{Raw: raw, Elapsed: elapsed(started), ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano), Valid: valid})
	if err == nil {
		err = f.Sync()
	}
	return err
}

func writeGap(r *Recorder, f *os.File, started time.Time, reason string) error {
	if err := r.write(Receipt{Kind: "gap", Reason: reason, Elapsed: elapsed(started), ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano), Valid: false}); err != nil {
		return err
	}
	return f.Sync()
}

func writeManifest(output string, manifest CaptureManifest) error {
	path := output + ".manifest.json"
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Link(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return nil
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func elapsed(start time.Time) string { return seconds(time.Since(start)) }

func seconds(d time.Duration) string {
	return fmt.Sprintf("%d.%09d", d/time.Second, d%time.Second)
}

func finiteDuration(d time.Duration) bool {
	return d > 0 && !math.IsInf(d.Seconds(), 0) && !math.IsNaN(d.Seconds())
}

func isPair(s string) bool {
	if strings.Count(s, "/") != 1 {
		return false
	}
	parts := strings.SplitN(s, "/", 2)
	return parts[0] != "" && parts[1] != "" && !strings.ContainsAny(s, " \t\r\n")
}

func pauseUntil(ctx context.Context, deadline time.Time, d time.Duration) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return true
	}
	if d > remaining {
		d = remaining
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
