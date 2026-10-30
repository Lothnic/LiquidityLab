package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/liquiditylab/liquiditylab/internal/marketdata"
)

const maxRecordingEntries = 10000

func validatePaperRecordingRoot(opt paperServerOptions) error {
	if opt.recordingRoot != filepath.Clean(opt.recordingRoot) {
		return errors.New("recording-root must be a clean absolute directory path")
	}
	if opt.metricsOutput == "" {
		return nil
	}
	root, err := filepath.EvalSymlinks(opt.recordingRoot)
	if err != nil {
		return errors.New("recording-root cannot be resolved")
	}
	metrics, err := prospectiveOutputPath(opt.metricsOutput)
	if err != nil {
		return errors.New("paper metrics output cannot be resolved")
	}
	rel, err := filepath.Rel(root, metrics)
	if err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("paper metrics output must be outside recording-root")
	}
	return nil
}

type paperRecordingAdmission struct {
	root   *os.Root
	lock   *os.File
	output string
}

func (a *paperRecordingAdmission) Close() error {
	return errors.Join(a.lock.Close(), a.root.Close())
}

// Admission is an exclusive cooperating-writer contract, not a filesystem quota.
// Operators must keep the root and its ancestors stable and must not mutate its
// contents while a daemon holds the directory lock. Existing files are never deleted.
func admitPaperRecording(path string, limit, captureBytes int64) (*paperRecordingAdmission, error) {
	refuse := func(reason string) error { return fmt.Errorf("recording storage admission refused: %s", reason) }
	reserve := captureBytes + 2*marketdata.MaxManifestBytes
	if !filepath.IsAbs(path) || path != filepath.Clean(path) || captureBytes < 1024 || captureBytes > 1<<30 || limit < reserve {
		return nil, refuse("invalid root or insufficient aggregate budget for one capture")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, refuse("root must be an existing real directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, refuse("cannot open root")
	}
	lock, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, refuse("cannot open directory lock")
	}
	a := &paperRecordingAdmission{root: root, lock: lock}
	admitted := false
	defer func() {
		if !admitted {
			_ = a.Close()
		}
	}()
	if err := lockPaperRecordingDirectory(lock); err != nil {
		return nil, refuse("exclusive directory lock unavailable or unsupported")
	}
	_, entries, err := scanPaperRecordings(root, limit-reserve)
	if err != nil {
		return nil, refuse("root inspection failed or aggregate byte/entry budget exhausted")
	}
	// Count the new directory and future raw/manifest/temp entries too.
	if entries+4 > maxRecordingEntries {
		return nil, refuse("entry budget exhausted")
	}
	name := "kraken-" + rand.Text()
	if err := root.Mkdir(name, 0o700); err != nil {
		return nil, refuse("cannot create fresh capture directory")
	}
	a.output = filepath.Join(path, name, "raw.jsonl")
	admitted = true
	return a, nil
}

// Read small batches instead of loading an arbitrarily large directory listing.
func scanPaperRecordings(root *os.Root, budget int64) (used int64, entries int, err error) {
	entries = 1 // root
	pending := []string{"."}
	for len(pending) > 0 {
		name := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		dir, err := root.Open(name)
		if err != nil {
			return used, entries, err
		}
		for {
			batch, readErr := dir.ReadDir(128)
			for _, entry := range batch {
				entries++
				if entries > maxRecordingEntries {
					dir.Close()
					return used, entries, errors.New("entry limit")
				}
				info, err := entry.Info()
				if err != nil {
					dir.Close()
					return used, entries, err
				}
				if info.IsDir() {
					pending = append(pending, filepath.Join(name, entry.Name()))
					continue
				}
				if !info.Mode().IsRegular() {
					dir.Close()
					return used, entries, errors.New("nonregular entry")
				}
				if info.Size() < 0 || info.Size() > budget-used {
					dir.Close()
					return used, entries, errors.New("byte limit")
				}
				used += info.Size()
			}
			if readErr != nil {
				closeErr := dir.Close()
				if readErr != io.EOF {
					return used, entries, readErr
				}
				if closeErr != nil {
					return used, entries, closeErr
				}
				break
			}
		}
	}
	return used, entries, nil
}
