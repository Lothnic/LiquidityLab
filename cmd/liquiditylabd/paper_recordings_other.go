//go:build !linux

package main

import (
	"errors"
	"os"
)

func lockPaperRecordingDirectory(file *os.File) error {
	return errors.New("aggregate recording admission requires Linux directory flock")
}
