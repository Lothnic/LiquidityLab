// Package marketarchive imports bounded public recordings into an immutable,
// content-addressed archive. Integrity verification does not authenticate a venue.
package marketarchive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/liquiditylab/liquiditylab/internal/marketdata"
)

const (
	Schema                     = "liquiditylab.marketarchive.v1"
	NormalizationVersion       = "kraken-normalizer.v1"
	MaxRawBytes          int64 = 64 << 20
	MaxManifestBytes     int64 = 64 << 10
	MaxLineBytes               = 8 << 20
	MaxEvents                  = 100000
	MaxNormalizedBytes         = 64 << 20
)

type Config struct {
	RawPath, ManifestPath, ArchiveRoot string
	// Zero selects MaxRawBytes; callers may lower but cannot raise the bound.
	MaxRawBytes int64
}

type Fingerprint struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type Source struct {
	Schema    string `json:"schema"`
	SessionID string `json:"session_id"`
	Venue     string `json:"venue"`
	Endpoint  string `json:"endpoint"`
	Symbol    string `json:"symbol"`
	Depth     int    `json:"depth"`
}

type Gap struct {
	SourceReceiptIndex uint64 `json:"source_receipt_index"`
	ElapsedNS          int64  `json:"elapsed_ns"`
	Reason             string `json:"reason"`
}

// Manifest is a deterministic, path-free summary of the checked source bytes.
// Continuous describes the sealed capture's integrity and continuity. It does
// not establish replay episode eligibility or venue authenticity. Declared
// source clock times are metadata; receipt elapsed time determines causality.
type Manifest struct {
	Schema                          string      `json:"schema"`
	ID                              string      `json:"id"`
	NormalizationVersion            string      `json:"normalization_version"`
	Source                          Source      `json:"source"`
	Raw                             Fingerprint `json:"raw"`
	SourceManifest                  Fingerprint `json:"source_manifest"`
	Normalized                      Fingerprint `json:"normalized"`
	CaptureStatus                   string      `json:"capture_status"`
	DeclaredStartedAt               string      `json:"declared_started_at"`
	DeclaredFinishedAt              string      `json:"declared_finished_at"`
	Receipts                        uint64      `json:"receipts"`
	RawFrames                       uint64      `json:"raw_frames"`
	BookMessages                    uint64      `json:"book_messages"`
	Snapshots                       uint64      `json:"snapshots"`
	Trades                          uint64      `json:"trades"`
	EventCount                      uint64      `json:"event_count"`
	FirstSnapshotElapsedNS          int64       `json:"first_snapshot_elapsed_ns"`
	FirstSnapshotSourceReceiptIndex uint64      `json:"first_snapshot_source_receipt_index"`
	CaptureEndElapsedNS             int64       `json:"capture_end_elapsed_ns"`
	CoverageEndNS                   int64       `json:"coverage_end_ns"`
	TerminalValid                   bool        `json:"terminal_valid"`
	Continuous                      bool        `json:"continuous"`
	Gaps                            []Gap       `json:"gaps"`
}

type Recording struct {
	Manifest Manifest
	Events   []marketdata.Event
}

func fingerprint(b []byte) Fingerprint {
	h := sha256.Sum256(b)
	return Fingerprint{hex.EncodeToString(h[:]), int64(len(b))}
}

func contentID(raw, manifest Fingerprint) string {
	return fingerprint([]byte(Schema + "\n" + raw.SHA256 + "\n" + manifest.SHA256 + "\n" + NormalizationVersion + "\n")).SHA256
}

func ValidID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Import reads each input exactly once, validates and normalizes those bytes,
// then promotes a fully fsynced owned directory. Existing artifacts are never
// replaced, and duplicate imports reverify every existing byte before success.
func Import(ctx context.Context, cfg Config) (Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	limit := cfg.MaxRawBytes
	if limit == 0 {
		limit = MaxRawBytes
	}
	if limit < 1 || limit > MaxRawBytes {
		return Manifest{}, errors.New("raw byte bound must be between 1 and 64MiB")
	}
	if cfg.RawPath == "" || cfg.ManifestPath == "" || cfg.ArchiveRoot == "" {
		return Manifest{}, errors.New("raw, manifest, and archive paths are required")
	}
	raw, err := readFile(ctx, cfg.RawPath, limit)
	if err != nil {
		return Manifest{}, fmt.Errorf("raw: %w", err)
	}
	source, err := readFile(ctx, cfg.ManifestPath, MaxManifestBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("source manifest: %w", err)
	}
	recording, normalized, err := verify(ctx, raw, source)
	if err != nil {
		return Manifest{}, err
	}
	m := recording.Manifest
	if err := initializeRoot(ctx, cfg.ArchiveRoot); err != nil {
		return Manifest{}, err
	}
	destination := filepath.Join(cfg.ArchiveRoot, m.ID)
	if _, err := os.Lstat(destination); err == nil {
		return verifyDuplicate(ctx, cfg.ArchiveRoot, m)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	tmp, err := os.MkdirTemp(cfg.ArchiveRoot, ".import-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(tmp)
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	manifest = append(manifest, '\n')
	for _, file := range []struct {
		name string
		data []byte
	}{
		{"raw.jsonl", raw}, {"source-manifest.json", source}, {"events.jsonl", normalized}, {"archive-manifest.json", manifest},
	} {
		if err := writeFile(ctx, filepath.Join(tmp, file.name), file.data); err != nil {
			return Manifest{}, err
		}
	}
	if err := syncDir(tmp); err != nil {
		return Manifest{}, err
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	// RENAME_NOREPLACE is required: ordinary rename can replace an empty
	// artifact directory and therefore cannot preserve immutability.
	err = promote(tmp, destination)
	if errors.Is(err, os.ErrExist) {
		return verifyDuplicate(ctx, cfg.ArchiveRoot, m)
	}
	if err != nil {
		return Manifest{}, err
	}
	if err := syncDir(cfg.ArchiveRoot); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func verifyDuplicate(ctx context.Context, root string, wanted Manifest) (Manifest, error) {
	existing, err := Load(ctx, root, wanted.ID)
	if err != nil {
		return Manifest{}, fmt.Errorf("existing artifact is invalid: %w", err)
	}
	if !reflect.DeepEqual(existing.Manifest, wanted) {
		return Manifest{}, errors.New("existing artifact conflicts with import")
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	// Another importer may have promoted the directory but not yet fsynced
	// its parent. Duplicate success must establish that durability itself,
	// including retries after an earlier parent-directory fsync failure.
	if err := syncDir(root); err != nil {
		return Manifest{}, err
	}
	return existing.Manifest, nil
}

// Load accepts an archive root and strict content ID, never an arbitrary
// artifact path. It reconstructs the summary and canonical events from checked
// source bytes and compares all stored artifacts before returning any events.
func Load(ctx context.Context, root, id string) (Recording, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if root == "" || !ValidID(id) {
		return Recording{}, errors.New("archive root and lowercase 64-hex dataset ID are required")
	}
	path := filepath.Join(root, id)
	read, closeArchive, err := openArchive(ctx, path)
	if err != nil {
		return Recording{}, err
	}
	defer closeArchive()
	manifestBytes, err := read("archive-manifest.json", MaxManifestBytes)
	if err != nil {
		return Recording{}, err
	}
	var stored Manifest
	if err := decode(manifestBytes, &stored); err != nil {
		return Recording{}, err
	}
	if stored.ID != id || stored.Schema != Schema || stored.NormalizationVersion != NormalizationVersion {
		return Recording{}, errors.New("archive manifest identity mismatch")
	}
	raw, err := read("raw.jsonl", MaxRawBytes)
	if err != nil {
		return Recording{}, err
	}
	source, err := read("source-manifest.json", MaxManifestBytes)
	if err != nil {
		return Recording{}, err
	}
	norm, err := read("events.jsonl", MaxNormalizedBytes)
	if err != nil {
		return Recording{}, err
	}
	if fingerprint(raw) != stored.Raw || fingerprint(source) != stored.SourceManifest || fingerprint(norm) != stored.Normalized {
		return Recording{}, errors.New("archive content hash or byte count mismatch")
	}
	actual, canonical, err := verify(ctx, raw, source)
	if err != nil {
		return Recording{}, err
	}
	if !reflect.DeepEqual(actual.Manifest, stored) || !bytes.Equal(norm, canonical) {
		return Recording{}, errors.New("archive reconstruction mismatch")
	}
	canonicalManifest, err := json.MarshalIndent(actual.Manifest, "", "  ")
	if err != nil {
		return Recording{}, err
	}
	if !bytes.Equal(manifestBytes, append(canonicalManifest, '\n')) {
		return Recording{}, errors.New("archive manifest is not its immutable canonical encoding")
	}
	return actual, nil
}

func readOpened(ctx context.Context, f *os.File, limit int64) ([]byte, error) {
	defer f.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("artifact must be a regular file")
	}
	if info.Size() > limit {
		return nil, errors.New("artifact byte limit exceeded")
	}
	var out bytes.Buffer
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := f.Read(buffer[:min(int64(len(buffer)), limit+1-int64(out.Len()))])
		out.Write(buffer[:n])
		if int64(out.Len()) > limit {
			return nil, errors.New("artifact byte limit exceeded")
		}
		if err == io.EOF {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func writeFile(ctx context.Context, path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := f.Write(data[:min(len(data), 64<<10)])
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// initializeRoot persists every directory name which MkdirAll could have
// created. Sync the complete absolute ancestor chain because a concurrent
// importer may have created a directory before this import observed it.
// Syncing only directories this caller believed missing would miss that race.
func initializeRoot(ctx context.Context, root string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return err
	}
	for current := absolute; ; current = filepath.Dir(current) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncDir(current); err != nil {
			return fmt.Errorf("persist archive directory %q: %w", current, err)
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

func decode(b []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func pair(s string) bool {
	parts := strings.Split(s, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != "" && len(s) <= 128 && !strings.ContainsAny(s, " \t\r\n")
}
