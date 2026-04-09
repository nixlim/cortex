// Package opslog writes structured JSONL operational-log lines to
// ~/.cortex/ops.log.
//
// Each written event carries the eight required fields from the spec's
// Logging section: timestamp, level, invocation_id, component, tx,
// entity_ids, message, error. The writer serializes appends through a
// mutex so concurrent callers from different goroutines each produce
// exactly one line per call, and it rotates the file to ops.log.1 when
// it would grow past ops_log.max_size_mb.
//
// The file mode is 0600 per the security section of the spec.
package opslog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Level is the structured log level.
type Level string

const (
	LevelDebug Level = "DEBUG"
	LevelInfo  Level = "INFO"
	LevelWarn  Level = "WARN"
	LevelError Level = "ERROR"
)

// FileMode is the mandatory file mode for ops.log per spec.
const FileMode os.FileMode = 0o600

// Event is one structured log line. All eight required fields are present
// on every line; optional fields are serialized as null or empty strings.
type Event struct {
	Timestamp    string   `json:"timestamp"`
	Level        Level    `json:"level"`
	InvocationID string   `json:"invocation_id"`
	Component    string   `json:"component"`
	Tx           string   `json:"tx"`
	EntityIDs    []string `json:"entity_ids"`
	Message      string   `json:"message"`
	Error        string   `json:"error"`
}

// Writer is a concurrency-safe ops.log writer.
type Writer struct {
	mu           sync.Mutex
	path         string
	invocationID string
	maxBytes     int64
	// now is swappable for tests.
	now func() time.Time
}

// Options configure a Writer.
type Options struct {
	// Path is the target file path (usually ~/.cortex/ops.log).
	Path string
	// InvocationID is the ULID assigned at command start. Every event
	// emitted by this writer inherits it.
	InvocationID string
	// MaxSizeMB is the rotation threshold in megabytes. 0 selects the
	// spec default of 50 MB.
	MaxSizeMB int
}

// New returns a new Writer. It does not touch the filesystem until the
// first Write; NewWriter is cheap.
func New(opts Options) (*Writer, error) {
	if opts.Path == "" {
		return nil, errors.New("opslog: path is required")
	}
	if opts.InvocationID == "" {
		return nil, errors.New("opslog: invocation_id is required")
	}
	max := opts.MaxSizeMB
	if max <= 0 {
		max = 50
	}
	return &Writer{
		path:         opts.Path,
		invocationID: opts.InvocationID,
		maxBytes:     int64(max) * 1024 * 1024,
		now:          time.Now,
	}, nil
}

// Write appends a single event line to ops.log. If the file would exceed
// maxBytes after this line, the file is first rotated to path+".1" and a
// fresh file is created. Concurrent calls are serialized.
func (w *Writer) Write(ev Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if ev.Timestamp == "" {
		ev.Timestamp = w.now().UTC().Format(time.RFC3339Nano)
	}
	if ev.Level == "" {
		ev.Level = LevelInfo
	}
	ev.InvocationID = w.invocationID
	if ev.EntityIDs == nil {
		ev.EntityIDs = []string{}
	}

	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("opslog: marshal: %w", err)
	}
	line = append(line, '\n')

	if err := w.ensureParentDir(); err != nil {
		return err
	}

	// Check current size and rotate if (size + len(line)) > maxBytes.
	if info, statErr := os.Stat(w.path); statErr == nil {
		if info.Size()+int64(len(line)) > w.maxBytes {
			if err := w.rotateLocked(); err != nil {
				return err
			}
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("opslog: stat: %w", statErr)
	}

	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, FileMode)
	if err != nil {
		return fmt.Errorf("opslog: open: %w", err)
	}
	defer f.Close()
	// Ensure existing files also have the tight mode (idempotent).
	_ = os.Chmod(w.path, FileMode)
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("opslog: append: %w", err)
	}
	return nil
}

func (w *Writer) ensureParentDir() error {
	dir := filepath.Dir(w.path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o700)
}

// rotateLocked moves w.path to w.path+".1", replacing any existing .1
// file. The caller must hold w.mu.
func (w *Writer) rotateLocked() error {
	rotated := w.path + ".1"
	if _, err := os.Stat(rotated); err == nil {
		if err := os.Remove(rotated); err != nil {
			return fmt.Errorf("opslog: remove old rotation: %w", err)
		}
	}
	if err := os.Rename(w.path, rotated); err != nil {
		return fmt.Errorf("opslog: rotate: %w", err)
	}
	return nil
}

// Close is a no-op for API symmetry; Writer does not hold a file handle
// between Write calls.
func (w *Writer) Close() error { return nil }

// CopyRemaining exists so tests (or callers) can tee the last file
// contents to an io.Writer without reopening.
func (w *Writer) CopyRemaining(dst io.Writer) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	data, err := os.ReadFile(w.path)
	if err != nil {
		return err
	}
	_, err = dst.Write(data)
	return err
}
