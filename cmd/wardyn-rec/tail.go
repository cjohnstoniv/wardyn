// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

// Tail-and-upload. A run that lives for weeks would otherwise deliver its cast
// only when the agent exits (never, if the sandbox is lost first) and as one
// PUT the control plane refuses past 64 MiB. So while asciinema records,
// wardyn-rec tails the cast file and uploads it in parts: every partInterval,
// or as soon as a part's worth is waiting. Each part is the cast's own header
// line followed by whole event lines, so each is a valid asciicast v2 document
// and no event (nor a secret inside one) is split between two uploads, which
// the control plane masks one at a time. Part 1 goes to the upload URL itself,
// the one request a short run has always made; part n >= 2 goes to
// <url>/parts/<n>, and the control plane joins them (recording.OpenJoined).
// The cast file itself is left to grow; this only tracks how much of it is sent.
var (
	// partMaxBytes is the control plane's per-upload cap (maxRecordingUploadBytes
	// in internal/api), header included.
	partMaxBytes int64 = 64 << 20
	partInterval       = 24 * time.Hour
	tailPoll           = 30 * time.Second
	// tailRetry spaces the attempts after a refused part, so an unreachable
	// control plane is not re-sent up to 64 MiB on every poll.
	tailRetry = 10 * time.Minute
)

type tailUploader struct {
	cast, url, token string
	header           []byte    // the cast's first line, newline included; nil until written
	off              int64     // offset of the first cast byte not yet uploaded
	part             int       // the next part's number, from 1
	due              time.Time // when the interval trigger fires
	retryAt          time.Time // no mid-run attempt before this, after a refusal
	stop, done       chan struct{}
}

func startTailUploader(cast, url, token string) *tailUploader {
	t := &tailUploader{
		cast: cast, url: url, token: token, part: 1, due: time.Now().Add(partInterval),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go t.loop()
	return t
}

func (t *tailUploader) loop() {
	defer close(t.done)
	tick := time.NewTicker(tailPoll)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			// Mid-run errors stay quiet: this process's stderr is the agent's own
			// terminal (a tmux pane, for a long run). The final flush reports
			// whatever never landed.
			_ = t.flush(false)
		}
	}
}

// finish stops the tail and uploads everything still unsent, a last line with
// no newline included.
func (t *tailUploader) finish() error {
	close(t.stop)
	<-t.done
	return t.flush(true)
}

// flush uploads the parts that are due: every waiting byte when final, else a
// part's worth of whole lines, or any whole lines once the interval is due.
func (t *tailUploader) flush(final bool) error {
	if !final && time.Now().Before(t.retryAt) {
		return nil
	}
	f, err := os.Open(t.cast)
	if err != nil {
		return fmt.Errorf("open cast: %w", err)
	}
	defer f.Close()
	if t.header == nil {
		h, err := bufio.NewReader(io.LimitReader(f, partMaxBytes)).ReadBytes('\n')
		if err != nil {
			if !final {
				return nil
			}
			// No complete header line at exit: send the file as it stands.
			return uploadCast(t.cast, t.url, t.token)
		}
		t.header, t.off = h, int64(len(h))
	}
	for {
		info, err := f.Stat()
		if err != nil {
			return fmt.Errorf("stat cast: %w", err)
		}
		pending := info.Size() - t.off
		room := partMaxBytes - int64(len(t.header))
		if !(final && (pending > 0 || t.part == 1) ||
			!final && pending >= room ||
			!final && pending > 0 && !time.Now().Before(t.due)) {
			return nil
		}
		n := min(pending, room)
		body := make([]byte, int64(len(t.header))+n)
		copy(body, t.header)
		if _, err := f.ReadAt(body[len(t.header):], t.off); err != nil {
			return fmt.Errorf("read cast: %w", err)
		}
		if !final || pending > room {
			cut := bytes.LastIndexByte(body[len(t.header):], '\n') + 1
			if cut == 0 {
				if pending >= room {
					return errors.New("an event line exceeds the upload cap")
				}
				return nil // only a partial line so far
			}
			body = body[:len(t.header)+cut]
		}
		url := t.url
		if t.part > 1 {
			url += "/parts/" + strconv.Itoa(t.part)
		}
		if err := putCast(body, url, t.token); err != nil {
			t.retryAt = time.Now().Add(tailRetry)
			return fmt.Errorf("part %d: %w", t.part, err)
		}
		t.off += int64(len(body) - len(t.header))
		t.part++
		t.due = time.Now().Add(partInterval)
	}
}
