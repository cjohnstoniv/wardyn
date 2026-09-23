// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const tailHeader = `{"version":2,"width":80,"height":24,"timestamp":1700000000}` + "\n"

type upload struct {
	path string
	body string
}

// partServer records every PUT it receives, answering status (204 when 0).
type partServer struct {
	mu      sync.Mutex
	got     []upload
	status  int
	onFirst func()
	srv     *httptest.Server
}

func newPartServer(t *testing.T) *partServer {
	ps := &partServer{}
	ps.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ps.mu.Lock()
		ps.got = append(ps.got, upload{r.URL.Path, string(b)})
		first := len(ps.got) == 1 && ps.onFirst != nil
		status := ps.status
		ps.mu.Unlock()
		if first {
			ps.onFirst()
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(ps.srv.Close)
	return ps
}

func (ps *partServer) uploads() []upload {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return append([]upload(nil), ps.got...)
}

func shrinkParts(t *testing.T, maxBytes int64) {
	origMax, origPoll := partMaxBytes, tailPoll
	partMaxBytes, tailPoll = maxBytes, 20*time.Millisecond
	t.Cleanup(func() { partMaxBytes, tailPoll = origMax, origPoll })
}

// joinParts is the control plane's join (recording.OpenJoined): part 1 whole,
// later parts without their repeated header line.
func joinParts(t *testing.T, ups []upload) string {
	t.Helper()
	var b strings.Builder
	for i, u := range ups {
		if i == 0 {
			b.WriteString(u.body)
			continue
		}
		rest, ok := strings.CutPrefix(u.body, tailHeader)
		if !ok {
			t.Fatalf("part %d does not start with the cast header: %q", i+1, u.body)
		}
		b.WriteString(rest)
	}
	return b.String()
}

func eventLines(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(`[` + string(rune('0'+i)) + `.5,"o","event"]` + "\n")
	}
	return b.String()
}

func writeCast(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// TestTailUploader_SizeTriggerCutsWholeLinesUnderTheCap: once a part's worth
// is waiting, it goes up as the header plus whole event lines, never more than
// partMaxBytes; part 1 on the bare URL, part n on /parts/n. A trailing partial
// line waits for the final flush, which sends everything.
func TestTailUploader_SizeTriggerCutsWholeLinesUnderTheCap(t *testing.T) {
	ev := `[0.5,"o","event"]` + "\n"
	shrinkParts(t, int64(len(tailHeader)+2*len(ev)+len(ev)/2)) // room for two lines, not three
	ps := newPartServer(t)
	cast := filepath.Join(t.TempDir(), "r.cast")
	content := tailHeader + eventLines(7)
	writeCast(t, cast, content)

	tu := &tailUploader{cast: cast, url: ps.srv.URL + "/rec", part: 1, due: time.Now().Add(time.Hour)}
	if err := tu.flush(false); err != nil {
		t.Fatal(err)
	}
	ups := ps.uploads()
	if len(ups) != 3 {
		t.Fatalf("mid-run parts = %d, want 3 (7 lines, 2 per part; the last line waits)", len(ups))
	}
	for i, u := range ups {
		wantPath := "/rec"
		if i > 0 {
			wantPath = "/rec/parts/" + string(rune('1'+i))
		}
		if u.path != wantPath {
			t.Errorf("part %d path = %q, want %q", i+1, u.path, wantPath)
		}
		if int64(len(u.body)) > partMaxBytes || !strings.HasPrefix(u.body, tailHeader) || !strings.HasSuffix(u.body, "\n") {
			t.Errorf("part %d is not header + whole lines under the cap: %q", i+1, u.body)
		}
	}

	writeCast(t, cast, `[9.5,"o","no newline yet`)
	if err := tu.flush(false); err != nil {
		t.Fatal(err)
	}
	if n := len(ps.uploads()); n != 3 {
		t.Fatalf("a partial line under the cap was uploaded mid-run (%d parts)", n)
	}
	content += `[9.5,"o","no newline yet`
	if err := tu.flush(true); err != nil {
		t.Fatal(err)
	}
	if got := joinParts(t, ps.uploads()); got != content {
		t.Fatalf("joined parts =\n%q\nwant the cast file\n%q", got, content)
	}
}

// TestTailUploader_IntervalTrigger: under the size cap, waiting lines go up
// once the interval is due, and not before.
func TestTailUploader_IntervalTrigger(t *testing.T) {
	ps := newPartServer(t)
	cast := filepath.Join(t.TempDir(), "r.cast")
	writeCast(t, cast, tailHeader+eventLines(2))
	tu := &tailUploader{cast: cast, url: ps.srv.URL + "/rec", part: 1, due: time.Now().Add(time.Hour)}
	if err := tu.flush(false); err != nil || len(ps.uploads()) != 0 {
		t.Fatalf("uploaded before the interval: err=%v parts=%d", err, len(ps.uploads()))
	}
	tu.due = time.Now().Add(-time.Second)
	if err := tu.flush(false); err != nil {
		t.Fatal(err)
	}
	if ups := ps.uploads(); len(ups) != 1 || ups[0].body != tailHeader+eventLines(2) {
		t.Fatalf("interval upload = %+v", ups)
	}
	if !tu.due.After(time.Now().Add(partInterval - time.Minute)) {
		t.Errorf("the next interval was not re-armed: due %v", tu.due)
	}
	if err := tu.flush(true); err != nil || len(ps.uploads()) != 1 {
		t.Fatalf("final flush with nothing new sent a part: err=%v parts=%d", err, len(ps.uploads()))
	}
}

// TestTailUploader_HeaderOnlyCastStillDelivers: a run that printed nothing
// still delivers its cast at exit, as it always has.
func TestTailUploader_HeaderOnlyCastStillDelivers(t *testing.T) {
	ps := newPartServer(t)
	cast := filepath.Join(t.TempDir(), "r.cast")
	writeCast(t, cast, tailHeader)
	tu := &tailUploader{cast: cast, url: ps.srv.URL + "/rec", part: 1, due: time.Now().Add(time.Hour)}
	if err := tu.flush(true); err != nil {
		t.Fatal(err)
	}
	if ups := ps.uploads(); len(ups) != 1 || ups[0].path != "/rec" || ups[0].body != tailHeader {
		t.Fatalf("header-only delivery = %+v", ups)
	}
}

// TestTailUploader_FailedPartWaitsBeforeRetry: a refused part is not re-sent
// on every poll, and the final flush still tries it.
func TestTailUploader_FailedPartWaitsBeforeRetry(t *testing.T) {
	shrinkParts(t, int64(len(tailHeader)+40))
	ps := newPartServer(t)
	ps.status = http.StatusBadGateway
	cast := filepath.Join(t.TempDir(), "r.cast")
	writeCast(t, cast, tailHeader+eventLines(4))
	tu := &tailUploader{cast: cast, url: ps.srv.URL + "/rec", part: 1, due: time.Now().Add(time.Hour)}
	if err := tu.flush(false); err == nil {
		t.Fatal("a refused part must report an error")
	}
	if err := tu.flush(false); err != nil || len(ps.uploads()) != 1 {
		t.Fatalf("retried at once: err=%v attempts=%d", err, len(ps.uploads()))
	}
	if err := tu.flush(true); err == nil || len(ps.uploads()) != 2 {
		t.Fatalf("final flush: err=%v attempts=%d, want an error after a second attempt", err, len(ps.uploads()))
	}
}

// TestRun_TailUploadsWhileAsciinemaRecords drives the real wiring with a stub
// asciinema that keeps recording until the control plane has received part 1,
// so the test fails unless a part goes up while the agent is still running.
func TestRun_TailUploadsWhileAsciinemaRecords(t *testing.T) {
	ev := `[0.5,"o","event"]` + "\n"
	shrinkParts(t, int64(len(tailHeader)+2*len(ev)))
	dir := t.TempDir()
	castDir := filepath.Join(dir, "cast")
	seen := filepath.Join(dir, "seen")
	ps := newPartServer(t)
	ps.onFirst = func() { _ = os.WriteFile(seen, nil, 0o600) }

	// argv: rec --stdin -q -c <cmd> <cast>
	stub := filepath.Join(dir, "asciinema")
	script := `#!/bin/sh
cast="$6"
printf '%s' '` + tailHeader + `' > "$cast"
for i in 0 1 2; do printf '[%s.5,"o","event"]\n' "$i" >> "$cast"; done
n=0
while [ ! -f '` + seen + `' ] && [ $n -lt 200 ]; do sleep 0.05; n=$((n+1)); done
if [ -f '` + seen + `' ]; then printf '[8.5,"o","after-part-1"]\n' >> "$cast"; fi
sh -c "$5"
`
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"-cast-dir", castDir, "-run", "run-tail", "-asciinema", stub,
		"-upload-url", ps.srv.URL + "/rec", "--", "true",
	}
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	ups := ps.uploads()
	cast, err := os.ReadFile(filepath.Join(castDir, "run-tail.cast"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(cast, []byte("after-part-1")) {
		t.Fatalf("no part reached the control plane while asciinema was recording (%d uploads)", len(ups))
	}
	if got := joinParts(t, ups); got != string(cast) {
		t.Fatalf("joined parts =\n%q\nwant the cast file\n%q", got, cast)
	}
}
