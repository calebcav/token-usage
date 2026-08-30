// Package sequence allocates metadata sequence numbers that remain increasing
// across concurrent plugin processes.
package sequence

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type Counter struct {
	Path string
	Now  func() time.Time
}

func (counter Counter) Next() (uint64, error) {
	if strings.TrimSpace(counter.Path) == "" {
		return 0, errors.New("sequence path is required")
	}
	if err := os.MkdirAll(filepath.Dir(counter.Path), 0o700); err != nil {
		return 0, errors.New("create plugin state directory")
	}
	file, err := os.OpenFile(counter.Path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, errors.New("open metadata sequence state")
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return 0, errors.New("lock metadata sequence state")
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN) //nolint:errcheck

	data, err := io.ReadAll(io.LimitReader(file, 64))
	if err != nil {
		return 0, errors.New("read metadata sequence state")
	}
	var previous uint64
	if value := strings.TrimSpace(string(data)); value != "" {
		previous, err = strconv.ParseUint(value, 10, 64)
		if err != nil {
			return 0, errors.New("metadata sequence state is malformed")
		}
	}

	now := time.Now
	if counter.Now != nil {
		now = counter.Now
	}
	candidate := uint64(max(int64(0), now().UnixNano()))
	if candidate <= previous {
		if previous == ^uint64(0) {
			return 0, errors.New("metadata sequence is exhausted")
		}
		candidate = previous + 1
	}
	if err := file.Truncate(0); err != nil {
		return 0, errors.New("write metadata sequence state")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, errors.New("write metadata sequence state")
	}
	if _, err := fmt.Fprintln(file, candidate); err != nil {
		return 0, errors.New("write metadata sequence state")
	}
	if err := file.Sync(); err != nil {
		return 0, errors.New("sync metadata sequence state")
	}
	return candidate, nil
}

var memory atomic.Uint64

func MemoryNext() uint64 {
	for {
		previous := memory.Load()
		candidate := uint64(max(int64(0), time.Now().UnixNano()))
		if candidate <= previous {
			candidate = previous + 1
		}
		if memory.CompareAndSwap(previous, candidate) {
			return candidate
		}
	}
}
