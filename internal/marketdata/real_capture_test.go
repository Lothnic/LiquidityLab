package marketdata

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifiedPublicCaptureHashChecksumAndGoldenNormalization(t *testing.T) {
	directory := filepath.Join("..", "..", "examples", "recordings", "kraken-btcusd-20261028")
	raw, err := os.ReadFile(filepath.Join(directory, "public-capture.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filepath.Join(directory, "public-capture.manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest CaptureManifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if manifest.SHA256 != hex.EncodeToString(sum[:]) || manifest.Bytes != int64(len(raw)) || manifest.Frames != 50 || manifest.ValidSnapshots != 1 || manifest.Gaps != 0 || manifest.Status != "frame_limit_reached" {
		t.Fatal("real public fixture differs from its verified source manifest")
	}
	actual, err := Normalize(filepath.Join(directory, "public-capture.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile(filepath.Join(directory, "normalized-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var expected []Event
	scanner := bufio.NewScanner(bytes.NewReader(golden))
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	// Empty and nil omitted sides have the same wire meaning; compare the full
	// typed serialization rather than incidental slice allocation state.
	actualJSON, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actualJSON, expectedJSON) {
		t.Fatal("real checksum-verified normalization differs from its golden")
	}
	counts := map[string]int{}
	for _, event := range actual {
		counts[event.Kind]++
	}
	if counts["snapshot"] != 1 || counts["book"] != 43 || counts["trade"] != 1 || counts["gap"] != 0 || len(actual) != 45 {
		t.Fatalf("real fixture event coverage differs: %v", counts)
	}
}
