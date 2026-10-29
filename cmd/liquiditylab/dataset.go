package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
)

func dataset(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: liquiditylab dataset <import|inspect> [flags]")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(stdout, "usage: liquiditylab dataset import --raw <capture.jsonl> --manifest <capture.manifest.json> --archive <root>")
		fmt.Fprintln(stdout, "       liquiditylab dataset inspect --archive <root> --id <lowercase-64-hex-ID>")
		return nil
	}
	fs := flag.NewFlagSet("dataset "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("archive", "", "operator archive root (required)")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch args[0] {
	case "import":
		raw := fs.String("raw", "", "raw JSONL capture (required)")
		manifest := fs.String("manifest", "", "capture manifest (required)")
		maxBytes := fs.Int64("max-raw-bytes", marketarchive.MaxRawBytes, "maximum raw bytes (1..64MiB)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("dataset import accepts flags only")
		}
		if *maxBytes <= 0 || *maxBytes > marketarchive.MaxRawBytes {
			return errors.New("max-raw-bytes must be between 1 and 64MiB")
		}
		m, err := marketarchive.Import(ctx, marketarchive.Config{RawPath: *raw, ManifestPath: *manifest, ArchiveRoot: *root, MaxRawBytes: *maxBytes})
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(m)
	case "inspect":
		id := fs.String("id", "", "lowercase 64-hex dataset ID (required)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("dataset inspect accepts flags only")
		}
		recording, err := marketarchive.Load(ctx, *root, *id)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(recording.Manifest)
	default:
		return fmt.Errorf("unknown dataset command %q (use import or inspect)", args[0])
	}
}
