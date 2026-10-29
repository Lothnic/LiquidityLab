//go:build !linux

package historicalreplay

import (
	"errors"
	"os"
)

func openSpec(string) (*os.File, error) {
	return nil, errors.New("historical replay spec IO currently requires Linux no-follow support")
}
