// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const outputLimit = 8 << 20
const outputChunk = 64 << 10

var errOutputOffset = errors.New("runner output: offset exceeds accepted output")

type outputPart struct {
	Offset int64 `json:"offset"`
	Size   int   `json:"size"`
}

type outputState struct {
	Ref      string       `json:"ref,omitempty"`
	End      int64        `json:"end"`
	Ack      int64        `json:"ack"`
	Parts    []outputPart `json:"parts"`
	Complete bool         `json:"complete"`
	Failure  string       `json:"failure,omitempty"`
}

type outputBuffer struct {
	mu          sync.Mutex
	dir         string
	state       outputState
	changed     chan struct{}
	stopped     error
	interrupted bool
}

func newOutputBuffer(dir string) (*outputBuffer, error) {
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("runner output: private directory required")
	}
	b := &outputBuffer{dir: dir, changed: make(chan struct{})}
	data, err := readPrivate(filepath.Join(dir, "state.json"), 32<<10)
	if errors.Is(err, os.ErrNotExist) {
		files, readErr := os.ReadDir(dir)
		if readErr != nil {
			return nil, readErr
		}
		for _, f := range files {
			if !strings.HasPrefix(f.Name(), ".state-") {
				return nil, errors.New("runner output: state missing for existing output")
			}
		}
		if err = b.save(); err != nil {
			return nil, err
		}
	} else {
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &b.state); err != nil {
			return nil, err
		}
		if err = b.validate(); err != nil {
			return nil, err
		}
		b.interrupted = !b.state.Complete
	}
	if err = b.reconcileFiles(); err != nil {
		return nil, err
	}
	return b, nil
}

func partName(offset int64) string { return fmt.Sprintf("%020d.bin", offset) }

func (b *outputBuffer) validate() error {
	s := b.state
	if s.End < 0 || s.Ack < 0 || s.Ack > s.End || len(s.Parts) > outputLimit/outputChunk+1 {
		return errors.New("runner output: invalid state")
	}
	next := s.End
	if len(s.Parts) > 0 {
		next = s.Parts[0].Offset
	}
	start := next
	for _, p := range s.Parts {
		if p.Offset != next || p.Offset < 0 || p.Size <= 0 || p.Size > outputChunk {
			return errors.New("runner output: invalid part range")
		}
		next += int64(p.Size)
	}
	if next != s.End || s.End-start > outputLimit || start > s.Ack {
		return errors.New("runner output: invalid retained range")
	}
	return nil
}

func readPrivate(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxBytes {
		return nil, errors.New("runner output: invalid private file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if int64(len(data)) > maxBytes {
		return nil, errors.New("runner output: file exceeds bound")
	}
	return data, err
}

func (b *outputBuffer) reconcileFiles() error {
	expected := map[string]int64{"state.json": -1}
	for _, p := range b.state.Parts {
		expected[partName(p.Offset)] = int64(p.Size)
	}
	files, err := os.ReadDir(b.dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		path := filepath.Join(b.dir, f.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("runner output: unexpected file type or permissions")
		}
		n, ok := expected[f.Name()]
		if !ok {
			// Orphans from interrupted commits have one of these two closed names.
			offset, parseErr := strconv.ParseInt(strings.TrimSuffix(f.Name(), ".bin"), 10, 64)
			if !strings.HasPrefix(f.Name(), ".state-") && (parseErr != nil || offset < 0 || partName(offset) != f.Name()) {
				return errors.New("runner output: unexpected file name")
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			continue
		}
		delete(expected, f.Name())
		if n < 0 {
			continue
		}
		if info.Size() < n {
			return errors.New("runner output: committed bytes are missing")
		}
		if info.Size() > n {
			if err := os.Truncate(path, n); err != nil {
				return err
			}
		}
	}
	if len(expected) != 0 {
		return errors.New("runner output: committed part is missing")
	}
	return nil
}

func (b *outputBuffer) save() error {
	data, err := json.Marshal(b.state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(b.dir, ".state-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(b.dir, "state.json")); err != nil {
		return err
	}
	dir, err := os.Open(b.dir)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (b *outputBuffer) notify() { close(b.changed); b.changed = make(chan struct{}) }
func (b *outputBuffer) start() int64 {
	if len(b.state.Parts) > 0 {
		return b.state.Parts[0].Offset
	}
	return b.state.End
}

func (b *outputBuffer) evict() error {
	if len(b.state.Parts) == 0 {
		return nil
	}
	p := b.state.Parts[0]
	if p.Offset+int64(p.Size) > b.state.Ack {
		return nil
	}
	b.state.Parts = b.state.Parts[1:]
	// Commit the retained range before unlinking: a crash may leave an orphan, never a referenced hole.
	if err := b.save(); err != nil {
		return err
	}
	return os.Remove(filepath.Join(b.dir, partName(p.Offset)))
}

func (b *outputBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for len(data) > 0 {
		if b.stopped != nil {
			return n, b.stopped
		}
		if b.state.Complete || b.interrupted {
			return n, io.ErrClosedPipe
		}
		room := outputLimit - int(b.state.End-b.start())
		if room == 0 {
			before := b.start()
			if err := b.evict(); err != nil {
				b.stopped = err
				b.notify()
				return n, err
			}
			if before != b.start() {
				continue
			}
			changed := b.changed
			b.mu.Unlock()
			<-changed
			b.mu.Lock()
			continue
		}
		take := min(len(data), room, outputChunk)
		written, err := b.appendPart(data[:take])
		n += written
		if err != nil {
			b.stopped = err
			b.notify()
			return n, err
		}
		data = data[take:]
		b.notify()
	}
	return n, nil
}

func (b *outputBuffer) appendPart(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		parts := b.state.Parts
		fresh := len(parts) == 0 || parts[len(parts)-1].Size == outputChunk
		p := outputPart{Offset: b.state.End}
		if !fresh {
			p = parts[len(parts)-1]
		}
		n := min(len(data), outputChunk-p.Size)
		flags := os.O_WRONLY | os.O_APPEND
		if fresh {
			flags |= os.O_CREATE | os.O_EXCL
		}
		f, err := os.OpenFile(filepath.Join(b.dir, partName(p.Offset)), flags, 0600)
		if err != nil {
			return written, err
		}
		_, err = f.Write(data[:n])
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return written, err
		}
		p.Size += n
		if fresh {
			b.state.Parts = append(parts, p)
		} else {
			b.state.Parts[len(parts)-1] = p
		}
		b.state.End += int64(n)
		if err = b.save(); err != nil {
			return written, err
		}
		written += n
		data = data[n:]
	}
	return written, nil
}

func (b *outputBuffer) ack(offset int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped != nil {
		return b.stopped
	}
	if offset < 0 || offset > b.state.End {
		return errOutputOffset
	}
	if offset <= b.state.Ack {
		return nil
	}
	b.state.Ack = offset
	if err := b.save(); err != nil {
		b.stopped = err
		b.notify()
		return err
	}
	b.notify()
	return nil
}

func (b *outputBuffer) read(ctx context.Context, offset int64, data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		if b.stopped != nil {
			return 0, b.stopped
		}
		if offset < b.start() {
			return 0, runner.ErrOutputUnrecoverable
		}
		if offset > b.state.End {
			return 0, errOutputOffset
		}
		for _, p := range b.state.Parts {
			if offset < p.Offset || offset >= p.Offset+int64(p.Size) {
				continue
			}
			f, err := os.Open(filepath.Join(b.dir, partName(p.Offset)))
			if err != nil {
				return 0, err
			}
			n, err := f.ReadAt(data[:min(len(data), int(p.Offset+int64(p.Size)-offset))], offset-p.Offset)
			return n, errors.Join(err, f.Close())
		}
		if b.stopped != nil {
			return 0, b.stopped
		}
		if b.state.Complete {
			if b.state.Failure != "" {
				return 0, errors.New(b.state.Failure)
			}
			return 0, io.EOF
		}
		if b.interrupted {
			return 0, runner.ErrOutputUnrecoverable
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			b.mu.Lock()
			return 0, ctx.Err()
		case <-changed:
		}
		b.mu.Lock()
	}
}

func (b *outputBuffer) BeginDrain() {}
func (b *outputBuffer) EndDrain(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state.Complete {
		return
	}
	b.state.Complete = true
	if err != nil {
		b.state.Failure = err.Error()
	}
	if err := b.save(); err != nil {
		b.stopped = err
	}
	b.notify()
}
func (b *outputBuffer) Close() error {
	b.EndDrain(nil)
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopped
}
