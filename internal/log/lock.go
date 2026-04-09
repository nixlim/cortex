// Package log implements the Cortex segmented append-only datom log.
//
// Physical layout (per cortex-spec.md §"Segmented Datom Log"):
//
//	~/.cortex/log.d/<ulid>-<writer-id>.jsonl   mode 0600
//	~/.cortex/log.d/                           mode 0700
//
// Each writer owns a current segment file and appends transaction groups
// to it under a per-segment advisory flock. The lock scope is minimal:
// the flock covers only the append and fsync, so backend writes and LLM
// calls never block a contending writer.
package log

import (
	"errors"
	"syscall"
	"time"
)

// ErrLockTimeout is returned by Append when the per-segment advisory flock
// cannot be acquired within the writer's configured lock timeout. When this
// error is returned no bytes have been written to the segment file.
var ErrLockTimeout = errors.New("log: segment lock timeout")

// lockPollInterval is how frequently tryLock retries a non-blocking flock
// attempt while waiting for a contended lock to clear. Kept small so that
// contention under the 5-second budget resolves promptly once the other
// writer releases.
const lockPollInterval = 20 * time.Millisecond

// acquireFlock takes an exclusive advisory lock on fd, returning nil on
// success or ErrLockTimeout if timeout elapsed without acquiring. The
// function uses non-blocking flock in a poll loop so that a caller's
// deadline is honoured with millisecond precision, matching SC-007's
// 5-second per-segment budget.
func acquireFlock(fd int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		// EWOULDBLOCK (EAGAIN) means another writer holds it. Any other
		// errno is a real failure (e.g. EBADF) and must not be masked as
		// a timeout.
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		if !time.Now().Before(deadline) {
			return ErrLockTimeout
		}
		time.Sleep(lockPollInterval)
	}
}

// releaseFlock drops the advisory lock on fd. Release failures are logged
// upstream but never override the caller's primary error.
func releaseFlock(fd int) error {
	return syscall.Flock(fd, syscall.LOCK_UN)
}
