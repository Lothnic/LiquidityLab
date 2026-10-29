package marketarchive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/liquiditylab/liquiditylab/internal/marketdata"
)

const retained = "../../examples/recordings/kraken-btcusd-20261028/"

func fixture(t *testing.T) ([]marketdata.Receipt, marketdata.CaptureManifest) {
	t.Helper()
	raw, err := os.ReadFile(retained + "public-capture.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var rows []marketdata.Receipt
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var row marketdata.Receipt
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	var m marketdata.CaptureManifest
	data, err := os.ReadFile(retained + "public-capture.manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return rows, m
}

func inputs(t *testing.T, rows []marketdata.Receipt, m marketdata.CaptureManifest) Config {
	t.Helper()
	var raw bytes.Buffer
	for _, row := range rows {
		if err := json.NewEncoder(&raw).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	fp := fingerprint(raw.Bytes())
	m.SHA256, m.Bytes = fp.SHA256, fp.Bytes
	manifest, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := Config{RawPath: filepath.Join(dir, "capture.jsonl"), ManifestPath: filepath.Join(dir, "capture.manifest.json"), ArchiveRoot: filepath.Join(dir, "archive")}
	if err := os.WriteFile(cfg.RawPath, raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.ManifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestRetainedCaptureRoundTrip(t *testing.T) {
	ctx := context.Background()
	cfg := Config{RawPath: retained + "public-capture.jsonl", ManifestPath: retained + "public-capture.manifest.json", ArchiveRoot: t.TempDir()}
	m, err := Import(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m.EventCount != 45 || m.Snapshots != 1 || m.BookMessages != 44 || m.Trades != 1 || !m.Continuous || !m.TerminalValid || len(m.Gaps) != 0 {
		t.Fatalf("unexpected summary: %+v", m)
	}
	r, err := Load(ctx, cfg.ArchiveRoot, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Manifest, m) || len(r.Events) != 45 || r.Events[0].Time != 0 || r.Events[0].ReceiptSessionID != m.Source.SessionID || r.Events[0].SourceReceiptIndex != m.FirstSnapshotSourceReceiptIndex {
		t.Fatalf("round trip lost identity: %+v", r.Manifest)
	}
	if m.DeclaredStartedAt != "2026-10-28T13:41:16.113155377Z" || m.DeclaredFinishedAt != "2026-10-28T13:41:20.035583041Z" {
		t.Fatal("declared source period metadata was lost")
	}
	retainedEvents, err := os.ReadFile(retained + "normalized-events.jsonl")
	if err != nil || fingerprint(retainedEvents) != m.Normalized {
		t.Fatalf("retained normalization conformance changed: %v", err)
	}
	if m.CoverageEndNS != m.CaptureEndElapsedNS-m.FirstSnapshotElapsedNS || m.CoverageEndNS <= r.Events[len(r.Events)-1].Time {
		t.Fatal("quiet tail not included in sealed coverage")
	}
	again, err := Import(ctx, cfg)
	if err != nil || !reflect.DeepEqual(again, m) {
		t.Fatalf("duplicate: %+v %v", again, err)
	}
	public, err := json.Marshal(m)
	if err != nil || bytes.Contains(public, []byte("/home/")) || bytes.Contains(public, []byte(`"output"`)) {
		t.Fatal("summary exposes original capture output path")
	}
	for _, name := range []string{"raw.jsonl", "source-manifest.json", "events.jsonl", "archive-manifest.json"} {
		if _, err := os.Stat(filepath.Join(cfg.ArchiveRoot, m.ID, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRejectInvalidCheckedSources(t *testing.T) {
	cases := map[string]func([]marketdata.Receipt, *marketdata.CaptureManifest) []marketdata.Receipt{
		"wrong schema": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			m.Schema = "other"
			return r
		},
		"wrong venue": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			m.Venue = "other"
			return r
		},
		"wrong symbol": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			m.Symbol = "ETH/USD"
			return r
		},
		"wrong depth": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			m.Depth = 25
			return r
		},
		"wrong endpoint": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			m.Endpoint = "https://example.invalid"
			return r
		},
		"null raw frame": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			r[0].Raw = "null"
			return r
		},
		"rejected subscription without gap": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			r[0].Raw = `{"method":"subscribe","success":false,"error":"rejected"}`
			return r
		},
		"session drift": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			r[6].SessionID = "changed"
			return r
		},
		"index drift": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			r[6].ReceiptIndex++
			return r
		},
		"negative elapsed": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			r[0].Elapsed = "-1"
			return r
		},
		"negative nanosecond elapsed": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			r[0].Elapsed = "-0.000000001"
			return r
		},
		"zero requested duration": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			m.RequestedDurationSeconds = "0"
			return r
		},
		"oversize requested duration": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			m.RequestedDurationSeconds = "86401"
			return r
		},
		"zero actual duration": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			m.DurationSeconds = "0"
			return r
		},
		"decreasing elapsed": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			r[6].Elapsed = "0"
			return r
		},
		"unknown kind": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			r[0].Kind = "unknown"
			return r
		},
		"missing end": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt { return r[:len(r)-1] },
		"duplicate end": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			return append(r, r[len(r)-1])
		},
		"post end": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			return append(r, r[0])
		},
		"end valid": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			r[len(r)-1].Valid = false
			return r
		},
		"manifest terminal": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			m.BookValidAtEnd = false
			return r
		},
		"prebook valid": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			r[0].Valid = true
			return r
		},
		"prebook trade snapshot valid": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			r[0].Raw = `{"channel":"trade","type":"snapshot","data":[{"symbol":"BTC/USD","price":"99","qty":"1","side":"buy","trade_id":1}]}`
			r[0].Valid = true
			return r
		},
		"snapshot invalid flag": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			for i := range r {
				if strings.Contains(r[i].Raw, `"channel":"book"`) && strings.Contains(r[i].Raw, `"type":"snapshot"`) {
					r[i].Valid = false
					break
				}
			}
			return r
		},
		"checksum changed paired hash": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			for i := range r {
				if strings.Contains(r[i].Raw, `"checksum":`) {
					var msg map[string]any
					_ = json.Unmarshal([]byte(r[i].Raw), &msg)
					msg["data"].([]any)[0].(map[string]any)["checksum"] = 0
					data, _ := json.Marshal(msg)
					r[i].Raw = string(data)
					break
				}
			}
			return r
		},
		"frames": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt { m.Frames--; return r },
		"book messages": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			m.BookMessages++
			return r
		},
		"snapshots": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			m.ValidSnapshots++
			return r
		},
		"gaps": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt { m.Gaps++; return r },
		"incomplete": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			m.Status = "incomplete"
			return r
		},
		"manifest error": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			m.Error = "failure"
			return r
		},
		"no snapshot": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			r = []marketdata.Receipt{r[0], r[len(r)-1]}
			r[1].ReceiptIndex = 1
			r[1].Valid = false
			m.BookValidAtEnd = false
			return r
		},
		"trade snapshot foreign symbol": func(r []marketdata.Receipt, m *marketdata.CaptureManifest) []marketdata.Receipt {
			r[0].Raw = `{"channel":"trade","type":"snapshot","data":[{"symbol":"ETH/USD"}]}`
			return r
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rows, m := fixture(t)
			cfg := inputs(t, mutate(rows, &m), m)
			if _, err := Import(context.Background(), cfg); err == nil {
				t.Fatal("invalid capture accepted")
			}
			entries, _ := os.ReadDir(cfg.ArchiveRoot)
			if len(entries) != 0 {
				t.Fatal("failed validation left archive artifacts")
			}
		})
	}
}

func TestSealedGapsAndQuietTail(t *testing.T) {
	for _, terminalGap := range []bool{false, true} {
		t.Run(map[bool]string{false: "before origin", true: "terminal invalid"}[terminalGap], func(t *testing.T) {
			rows, m := fixture(t)
			at := 1
			if terminalGap {
				at = len(rows) - 1
			}
			gap := marketdata.Receipt{Schema: m.Schema, SessionID: m.SessionID, Depth: m.Depth, Elapsed: rows[at].Elapsed, Kind: "gap", Reason: "connection_ended_resnapshot_required"}
			rows = append(rows[:at], append([]marketdata.Receipt{gap}, rows[at:]...)...)
			for i := range rows {
				rows[i].ReceiptIndex = uint64(i)
			}
			if terminalGap {
				rows[len(rows)-1].Valid = false
				m.BookValidAtEnd = false
			}
			rows[len(rows)-1].Elapsed = "8.000000000"
			m.DurationSeconds = "8.100000000"
			m.FinishedAt = "2026-10-28T13:41:24.213155377Z"
			m.Gaps = 1
			m.Status = "recorded_with_gaps"
			cfg := inputs(t, rows, m)
			out, err := Import(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if out.Continuous || len(out.Gaps) != 1 || out.TerminalValid == terminalGap || out.CoverageEndNS != 8_000_000_000-out.FirstSnapshotElapsedNS {
				t.Fatalf("gapped summary incorrect: %+v", out)
			}
			wantEvents := uint64(45)
			if terminalGap {
				wantEvents++
			}
			if out.EventCount != wantEvents {
				t.Fatal("quiet tail invented an event")
			}
		})
	}
}

func TestConcurrentDuplicateAndManifestIdentity(t *testing.T) {
	rows, m := fixture(t)
	cfg := inputs(t, rows, m)
	var wg sync.WaitGroup
	ids := make(chan string, 4)
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() { out, err := Import(context.Background(), cfg); ids <- out.ID; errs <- err })
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for value := range ids {
		if id != "" && id != value {
			t.Fatal("unstable ID")
		}
		id = value
	}
	entries, err := os.ReadDir(cfg.ArchiveRoot)
	if err != nil || len(entries) != 1 || entries[0].Name() != id {
		t.Fatalf("temporary artifacts left: %v %v", entries, err)
	}
	manifest, err := os.ReadFile(cfg.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.ManifestPath, append(manifest, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Import(context.Background(), cfg)
	if err != nil || second.ID == id {
		t.Fatalf("exact manifest bytes not pinned: %v", err)
	}
}

func TestNestedArchiveRootCreationAndDuplicate(t *testing.T) {
	rows, m := fixture(t)
	cfg := inputs(t, rows, m)
	cfg.ArchiveRoot = filepath.Join(t.TempDir(), "new", "nested", "archive")
	out, err := Import(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range []string{filepath.Dir(filepath.Dir(cfg.ArchiveRoot)), filepath.Dir(cfg.ArchiveRoot), cfg.ArchiveRoot} {
		info, err := os.Stat(current)
		if err != nil || !info.IsDir() {
			t.Fatalf("archive ancestor was not created: %s %v", current, err)
		}
	}
	loaded, err := Load(context.Background(), cfg.ArchiveRoot, out.ID)
	if err != nil || !reflect.DeepEqual(loaded.Manifest, out) {
		t.Fatalf("nested archive did not load: %v", err)
	}
	again, err := Import(context.Background(), cfg)
	if err != nil || !reflect.DeepEqual(again, out) {
		t.Fatalf("nested archive duplicate changed: %v", err)
	}
	entries, err := os.ReadDir(cfg.ArchiveRoot)
	if err != nil || len(entries) != 1 || entries[0].Name() != out.ID {
		t.Fatalf("nested archive leaked staging artifacts: %v %v", entries, err)
	}
}

func TestZeroFrameReconnectsAreValidSourceMetadata(t *testing.T) {
	rows, m := fixture(t)
	gap := marketdata.Receipt{Schema: m.Schema, SessionID: m.SessionID, Depth: m.Depth, Elapsed: rows[0].Elapsed, Kind: "gap", Reason: "connection_ended_resnapshot_required"}
	rows = []marketdata.Receipt{gap, rows[4], rows[len(rows)-1]}
	for i := range rows {
		rows[i].ReceiptIndex = uint64(i)
	}
	m.Frames, m.BookMessages, m.ValidSnapshots, m.Gaps, m.Connections = 1, 1, 1, 1, 2
	m.Status = "recorded_with_gaps"
	out, err := Import(context.Background(), inputs(t, rows, m))
	if err != nil || out.Continuous || !out.TerminalValid || out.EventCount != 1 {
		t.Fatalf("zero-frame reconnect history rejected: %+v %v", out, err)
	}
}

func TestChecksumGapIsRetained(t *testing.T) {
	rows, m := fixture(t)
	last := len(rows) - 2
	start := strings.Index(rows[last].Raw, `"checksum":`) + len(`"checksum":`)
	end := start
	for end < len(rows[last].Raw) && rows[last].Raw[end] >= '0' && rows[last].Raw[end] <= '9' {
		end++
	}
	rows[last].Raw = rows[last].Raw[:start] + "0" + rows[last].Raw[end:]
	rows[last].Valid = false
	gap := marketdata.Receipt{Schema: m.Schema, SessionID: m.SessionID, Depth: m.Depth, Elapsed: rows[last].Elapsed, Kind: "gap", Reason: "invalid Kraken book: checksum mismatch"}
	rows = append(rows[:last+1], append([]marketdata.Receipt{gap}, rows[last+1:]...)...)
	for i := range rows {
		rows[i].ReceiptIndex = uint64(i)
	}
	rows[len(rows)-1].Valid = false
	m.BookMessages--
	m.Gaps = 1
	m.BookValidAtEnd = false
	m.Status = "recorded_with_gaps"
	out, err := Import(context.Background(), inputs(t, rows, m))
	if err != nil || out.Continuous || out.TerminalValid || len(out.Gaps) != 2 {
		t.Fatalf("checksum gap was lost: %+v %v", out, err)
	}
	for _, g := range out.Gaps {
		if !strings.Contains(g.Reason, "checksum mismatch") {
			t.Fatalf("checksum reason was lost: %+v", g)
		}
	}
}

type cancelAtStaging struct {
	context.Context
	root      string
	triggered bool
}

func (c *cancelAtStaging) Err() error {
	if c.triggered {
		return context.Canceled
	}
	entries, _ := os.ReadDir(c.root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".import-") {
			c.triggered = true
			return context.Canceled
		}
	}
	return nil
}

func TestCancelledStagingIsRemovedAndEmptyDestinationNotReplaced(t *testing.T) {
	rows, m := fixture(t)
	cfg := inputs(t, rows, m)
	ctx := &cancelAtStaging{Context: context.Background(), root: cfg.ArchiveRoot}
	if _, err := Import(ctx, cfg); !errors.Is(err, context.Canceled) || !ctx.triggered {
		t.Fatalf("staging cancellation not exercised: %v", err)
	}
	entries, err := os.ReadDir(cfg.ArchiveRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled staging leaked artifacts: %v %v", entries, err)
	}
	out, err := Import(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(cfg.ArchiveRoot, out.ID)
	if err := os.RemoveAll(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(context.Background(), cfg); err == nil {
		t.Fatal("empty destination was overwritten")
	}
	entries, err = os.ReadDir(destination)
	if err != nil || len(entries) != 0 {
		t.Fatal("existing empty destination changed")
	}
}

func TestTamperingSymlinksAndStrictIDs(t *testing.T) {
	for _, filename := range []string{"raw.jsonl", "source-manifest.json", "events.jsonl", "archive-manifest.json"} {
		t.Run(filename, func(t *testing.T) {
			rows, m := fixture(t)
			cfg := inputs(t, rows, m)
			out, err := Import(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(cfg.ArchiveRoot, out.ID, filename)
			if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(context.Background(), cfg.ArchiveRoot, out.ID); err == nil {
				t.Fatal("tampered content loaded")
			}
			if _, err := Import(context.Background(), cfg); err == nil {
				t.Fatal("duplicate overwrote tampered content")
			}
			got, _ := os.ReadFile(path)
			if string(got) != "{}\n" {
				t.Fatal("tampered content overwritten")
			}
		})
	}
	rows, m := fixture(t)
	cfg := inputs(t, rows, m)
	out, err := Import(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", out.ID + "/raw.jsonl", "../" + out.ID, strings.ToUpper(out.ID), strings.Repeat("z", 64)} {
		if _, err := Load(context.Background(), cfg.ArchiveRoot, id); err == nil {
			t.Fatalf("unsafe ID accepted: %s", id)
		}
	}
	path := filepath.Join(cfg.ArchiveRoot, out.ID, "raw.jsonl")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cfg.RawPath, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), cfg.ArchiveRoot, out.ID); err == nil {
		t.Fatal("symlink artifact accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), cfg.ArchiveRoot, out.ID); err == nil {
		t.Fatal("nonregular artifact accepted")
	}
	other := t.TempDir()
	if err := os.Symlink(filepath.Join(cfg.ArchiveRoot, out.ID), filepath.Join(other, out.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), other, out.ID); err == nil {
		t.Fatal("symlink directory accepted")
	}
	if _, err := Import(context.Background(), Config{RawPath: cfg.RawPath, ManifestPath: cfg.ManifestPath, ArchiveRoot: other}); err == nil {
		t.Fatal("duplicate symlink directory accepted")
	}
}

func TestBoundsTornInputAndCancellation(t *testing.T) {
	rows, m := fixture(t)
	for name, alter := range map[string]func(Config) Config{
		"lower raw bound": func(c Config) Config { c.MaxRawBytes = 1; return c },
		"raise raw bound": func(c Config) Config { c.MaxRawBytes = MaxRawBytes + 1; return c },
		"torn final line": func(c Config) Config {
			b, _ := os.ReadFile(c.RawPath)
			_ = os.WriteFile(c.RawPath, b[:len(b)-1], 0o600)
			return c
		},
		"manifest size": func(c Config) Config {
			_ = os.WriteFile(c.ManifestPath, bytes.Repeat([]byte(" "), int(MaxManifestBytes+1)), 0o600)
			return c
		},
		"hash": func(c Config) Config {
			b, _ := os.ReadFile(c.RawPath)
			b[10] = 'x'
			_ = os.WriteFile(c.RawPath, b, 0o600)
			return c
		},
		"raw symlink": func(c Config) Config {
			link := c.RawPath + ".link"
			_ = os.Symlink(c.RawPath, link)
			c.RawPath = link
			return c
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := alter(inputs(t, rows, m))
			if _, err := Import(context.Background(), c); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	t.Run("oversize sparse file", func(t *testing.T) {
		cfg := inputs(t, rows, m)
		f, err := os.OpenFile(cfg.RawPath, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.Truncate(MaxRawBytes + 1); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if _, err := Import(context.Background(), cfg); err == nil {
			t.Fatal("oversize accepted")
		}
	})
	t.Run("line bound", func(t *testing.T) {
		r := append([]marketdata.Receipt(nil), rows...)
		r[0].Raw = strings.Repeat(" ", MaxLineBytes)
		cfg := inputs(t, r, m)
		if _, err := Import(context.Background(), cfg); err == nil {
			t.Fatal("oversize line accepted")
		}
	})
	t.Run("event fanout", func(t *testing.T) {
		r := append([]marketdata.Receipt(nil), rows...)
		r[0].Raw = `{"channel":"trade","type":"update","data":[` + strings.Repeat(`{"symbol":"BTC/USD"},`, MaxEvents) + `{"symbol":"BTC/USD"}]}`
		cfg := inputs(t, r, m)
		if _, err := Import(context.Background(), cfg); err == nil {
			t.Fatal("event fanout accepted")
		}
	})
	t.Run("cancelled import", func(t *testing.T) {
		cfg := inputs(t, rows, m)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Import(ctx, cfg); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
		entries, _ := os.ReadDir(cfg.ArchiveRoot)
		if len(entries) != 0 {
			t.Fatal("cancelled import left artifacts")
		}
	})
}
