//go:build !linux

package marketarchive

import (
	"context"
	"errors"
)

var errPlatform = errors.New("verified dataset archive currently requires Linux no-follow IO and atomic no-replace directory promotion")

func promote(string, string) error                            { return errPlatform }
func readFile(context.Context, string, int64) ([]byte, error) { return nil, errPlatform }
func openArchive(context.Context, string) (func(string, int64) ([]byte, error), func(), error) {
	return nil, nil, errPlatform
}
