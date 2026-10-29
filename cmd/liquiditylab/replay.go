package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/liquiditylab/liquiditylab/internal/historicalreplay"
)

func historicalReplay(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: liquiditylab replay compare --archive ROOT --id ID --spec SPEC.json --output NEW.json")
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stdout, "usage: liquiditylab replay compare --archive ROOT --id ID --spec SPEC.json --output NEW.json")
		return nil
	}
	if args[0] != "compare" {
		return fmt.Errorf("unknown replay command %q (use compare)", args[0])
	}
	fs := flag.NewFlagSet("replay compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("archive", "", "trusted operator archive root (required)")
	id := fs.String("id", "", "verified lowercase 64-hex dataset ID (required)")
	specPath := fs.String("spec", "", "complete explicit comparison specification JSON (required)")
	output := fs.String("output", "", "new report JSON path in an existing directory (required)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *root == "" || *id == "" || *specPath == "" || *output == "" {
		return errors.New("replay compare requires --archive, --id, --spec and --output, with flags only")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	spec, err := historicalreplay.ReadSpec(ctx, *specPath)
	if err != nil {
		return err
	}
	report, err := historicalreplay.Compare(ctx, *root, *id, spec)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := historicalreplay.Encode(report)
	if err != nil {
		return err
	}
	if err := writeReplayReport(ctx, *output, encoded); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "historical simulation comparison written to %s\n", *output)
	return err
}

// Publish only a complete fsynced file. A same-directory hard link creates the
// output exclusively; neither preexisting files nor symlinks are overwritten.
func writeReplayReport(ctx context.Context, path string, data []byte) error {
	if len(data) > historicalreplay.MaxReportBytes {
		return errors.New("report exceeds 16MiB")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".replay-report-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}()
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
	if err := f.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(tmp, path); err != nil {
		return err
	}
	// Once linked, preserve the complete artifact even if directory syncing
	// fails. A failure cannot justify removing a destination another operator
	// may already have inspected or replaced. The caller reports the error.
	if err := os.Remove(tmp); err != nil {
		return err
	}
	if err := syncReplayDirectory(dir); err != nil {
		return err
	}
	return nil
}

func syncReplayDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
