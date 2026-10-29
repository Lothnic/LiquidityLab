package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
)

func TestDatasetImportInspect(t *testing.T) {
	root := t.TempDir()
	fixture := "../../examples/recordings/kraken-btcusd-20261028/"
	var stdout, stderr bytes.Buffer
	if err := run([]string{"dataset", "import", "--raw", fixture + "public-capture.jsonl", "--manifest", fixture + "public-capture.manifest.json", "--archive", root}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var imported marketarchive.Manifest
	if err := json.Unmarshal(stdout.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.EventCount != 45 || !imported.Continuous || !marketarchive.ValidID(imported.ID) {
		t.Fatalf("unexpected imported manifest: %+v", imported)
	}
	first := stdout.String()
	stdout.Reset()
	if err := run([]string{"dataset", "inspect", "--archive", root, "--id", imported.ID}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != first || strings.Contains(stdout.String(), `"output"`) {
		t.Fatal("inspect changed summary or exposed source path")
	}
}

func TestDatasetArguments(t *testing.T) {
	for _, args := range [][]string{
		{"dataset"}, {"dataset", "unknown"}, {"dataset", "import"}, {"dataset", "inspect"},
		{"dataset", "import", "positional"},
		{"dataset", "inspect", "--archive", t.TempDir(), "--id", "../capture.jsonl"},
		{"dataset", "import", "--max-raw-bytes", "67108865"},
		{"dataset", "import", "--max-raw-bytes", "0"},
		{"dataset", "import", "--max-raw-bytes", "-1"},
	} {
		var stdout, stderr bytes.Buffer
		if err := run(args, &stdout, &stderr); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
		if stdout.Len() != 0 {
			t.Fatal("failed command produced success output")
		}
	}
	for _, args := range [][]string{{"help"}, {"dataset", "help"}} {
		var stdout, stderr bytes.Buffer
		if err := run(args, &stdout, &stderr); err != nil || !strings.Contains(stdout.String(), "dataset") {
			t.Fatalf("missing dataset help: %v", err)
		}
	}
}
