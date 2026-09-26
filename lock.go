package embeddeddsql

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on path (created if missing) that
// is shared across processes, so parallel test binaries never download or
// extract the same files at once. The returned function releases it.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("embedded-dsql: open lock file %s: %w", path, err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("embedded-dsql: lock %s: %w", path, err), f.Close())
	}
	return func() {
		// Closing the descriptor releases the lock; there is nothing useful
		// to do with a close error on a lock file.
		if err := f.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "embedded-dsql: release lock %s: %v\n", path, err)
		}
	}, nil
}
