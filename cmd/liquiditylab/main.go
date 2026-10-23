// Command liquiditylab provides bounded public-data and simulated paper tools.
// It has no private venue operations.
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
	"time"

	"github.com/liquiditylab/liquiditylab/internal/marketdata"
)

const defaultNormalizeLimit = 64 << 20

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "liquiditylab:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: liquiditylab <capture|normalize|paper> [flags]")
	}
	switch args[0] {
	case "capture":
		return capture(args[1:], stdout, stderr)
	case "normalize":
		return normalize(args[1:], stdout, stderr)
	case "paper":
		return paper(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, "usage: liquiditylab <capture|normalize|paper> [flags]")
		fmt.Fprintln(stdout, "  capture   record Kraken public book and trade streams")
		fmt.Fprintln(stdout, "  normalize validate a raw recording and emit causal JSONL events")
		fmt.Fprintln(stdout, "  paper     run a bounded recorded-data session with synthetic balances")
		return nil
	default:
		return fmt.Errorf("unknown command %q (use capture, normalize, or paper)", args[0])
	}
}

func capture(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	fs.SetOutput(stderr)
	symbol := fs.String("symbol", "BTC/USD", "Kraken v2 symbol, for example BTC/USD")
	output := fs.String("output", "", "new raw JSONL output path (required)")
	duration := fs.Duration("duration", 30*time.Second, "maximum capture duration")
	depth := fs.Int("depth", 10, "book depth: 10, 25, 100, 500, or 1000")
	maxFrames := fs.Uint64("max-frames", 10000, "maximum received WebSocket frames (1..1000000)")
	maxBytes := fs.Int64("max-bytes", 64<<20, "maximum raw capture bytes (1024..8GiB)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("capture accepts flags only")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	manifest, err := marketdata.Capture(ctx, marketdata.CaptureConfig{
		Symbol: *symbol, Output: *output, Duration: *duration,
		Depth: *depth, MaxFrames: *maxFrames, MaxBytes: *maxBytes,
	})
	if manifest.SessionID != "" {
		if e := json.NewEncoder(stdout).Encode(manifest); e != nil && err == nil {
			err = e
		}
	}
	return err
}

func normalize(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("normalize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "raw JSONL recording path (required)")
	output := fs.String("output", "", "new normalized JSONL output path (default: stdout)")
	maxBytes := fs.Int64("max-input-bytes", defaultNormalizeLimit, "maximum raw recording size accepted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("normalize accepts flags only")
	}
	if *input == "" {
		return errors.New("normalize requires --input")
	}
	if *maxBytes <= 0 {
		return errors.New("max-input-bytes must be positive")
	}
	info, err := os.Stat(*input)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("input must be a regular file")
	}
	if info.Size() > *maxBytes {
		return fmt.Errorf("recording is %d bytes, exceeding --max-input-bytes=%d", info.Size(), *maxBytes)
	}
	events, err := marketdata.Normalize(*input)
	if err != nil {
		return err
	}
	var dst io.Writer = stdout
	var f *os.File
	if *output != "" {
		f, err = os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		dst = f
		defer f.Close()
	}
	enc := json.NewEncoder(dst)
	for _, event := range events {
		if err := enc.Encode(event); err != nil {
			return err
		}
	}
	if f != nil {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	return nil
}
