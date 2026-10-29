package marketarchive

import (
	"context"
	"os"

	"golang.org/x/sys/unix"
)

func promote(tmp, destination string) error {
	return unix.Renameat2(unix.AT_FDCWD, tmp, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
}

func readFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return readOpened(ctx, os.NewFile(uintptr(fd), path), limit)
}

func openArchive(ctx context.Context, path string) (func(string, int64) ([]byte, error), func(), error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	read := func(name string, limit int64) ([]byte, error) {
		n, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		return readOpened(ctx, os.NewFile(uintptr(n), name), limit)
	}
	return read, func() { _ = unix.Close(fd) }, nil
}
