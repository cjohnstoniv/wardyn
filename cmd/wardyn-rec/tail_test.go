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
	"strconv"
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

// TestTailUploader_SizeCutPartLeavesMaskingHeadroom models the control plane's
// upload path (64 MiB before masking, the PG store's 64 MiB after it): a
// registered secret shorter than the placeholder lengthens every part it is in,
// so a part cut near the cap would be refused on every attempt and stall the
// rest of the run's recording behind it.
func TestTailUploader_SizeCutPartLeavesMaskingHeadroom(t *testing.T) {
	const cpCap = 64 << 20
	secret, mask := []byte("s3cr3t!!"), []byte("<secret-hidden>")
	var (
		mu       sync.Mutex
		accepted int
		got      int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cpCap))
		if err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		if len(bytes.ReplaceAll(b, secret, mask)) > cpCap {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		accepted++
		got += int64(len(b) - len(tailHeader))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	line := `[1.5,"o","token s3cr3t!! printed by the agent"]` + "\n"
	events := strings.Repeat(line, (cpCap+cpCap/4)/len(line))
	cast := filepath.Join(t.TempDir(), "r.cast")
	writeCast(t, cast, tailHeader+events)

	tu := &tailUploader{cast: cast, url: srv.URL + "/rec", part: 1, due: time.Now().Add(time.Hour)}
	if err := tu.flush(true); err != nil {
		t.Fatalf("a size-cut part was refused after masking: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != int64(len(events)) || accepted < 2 {
		t.Fatalf("delivered %d of %d event bytes in %d parts", got, len(events), accepted)
	}
}

// TestTailUploader_OverCapLineWaitsBeforeRetry: an event line longer than a
// part is not re-read on every poll either.
func TestTailUploader_OverCapLineWaitsBeforeRetry(t *testing.T) {
	shrinkParts(t, int64(len(tailHeader)+8))
	ps := newPartServer(t)
	cast := filepath.Join(t.TempDir(), "r.cast")
	writeCast(t, cast, tailHeader+eventLines(1))
	tu := &tailUploader{cast: cast, url: ps.srv.URL + "/rec", part: 1, due: time.Now().Add(time.Hour)}
	if err := tu.flush(false); err == nil {
		t.Fatal("an over-cap event line must report an error")
	}
	if !tu.retryAt.After(time.Now()) {
		t.Fatalf("no retry wait after an over-cap line: retryAt %v", tu.retryAt)
	}
}

// TestTailUploader_SizeTriggerFiresAtExactlyAPartsWorth pins the size boundary:
// one byte short of a part's worth (header included) waits, and the byte that
// completes it sends a part of exactly partMaxBytes.
func TestTailUploader_SizeTriggerFiresAtExactlyAPartsWorth(t *testing.T) {
	ev := `[0.5,"o","event"]` + "\n"
	shrinkParts(t, int64(len(tailHeader)+3*len(ev)))
	ps := newPartServer(t)
	cast := filepath.Join(t.TempDir(), "r.cast")
	writeCast(t, cast, tailHeader+ev+ev+ev[:len(ev)-1])
	tu := &tailUploader{cast: cast, url: ps.srv.URL + "/rec", part: 1, due: time.Now().Add(time.Hour)}
	if err := tu.flush(false); err != nil || len(ps.uploads()) != 0 {
		t.Fatalf("one byte short of a part's worth: err=%v parts=%d, want nothing sent", err, len(ps.uploads()))
	}
	writeCast(t, cast, "\n")
	if err := tu.flush(false); err != nil {
		t.Fatal(err)
	}
	if ups := ps.uploads(); len(ups) != 1 || int64(len(ups[0].body)) != partMaxBytes || ups[0].body != tailHeader+ev+ev+ev {
		t.Fatalf("at exactly a part's worth: %+v, want one part of %d bytes", ups, partMaxBytes)
	}
}

// TestTailUploader_EveryPartReprefixesTheHeader walks both triggers at their
// real values' boundaries: the 24 h interval (twice), the size cap and the exit
// flush each send a part that is the cast's header line plus the next bytes,
// on its own address, and the parts join back to the cast file exactly.
func TestTailUploader_EveryPartReprefixesTheHeader(t *testing.T) {
	if partInterval != 24*time.Hour || partMaxBytes != (64<<20)/2 {
		t.Fatalf("partInterval %v, partMaxBytes %d; want 24h and half the control plane's 64 MiB upload cap",
			partInterval, partMaxBytes)
	}
	ev := func(s string) string { return `[1.5,"o","` + s + `"]` + "\n" }
	shrinkParts(t, int64(len(tailHeader)+len(ev("c1"))+len(ev("c2"))))
	ps := newPartServer(t)
	cast := filepath.Join(t.TempDir(), "r.cast")
	tu := &tailUploader{cast: cast, url: ps.srv.URL + "/rec", part: 1, due: time.Now().Add(time.Hour)}
	step := func(add string, dueNow, final bool) {
		t.Helper()
		writeCast(t, cast, add)
		if dueNow {
			tu.due = time.Now()
		}
		if err := tu.flush(final); err != nil {
			t.Fatal(err)
		}
	}
	step(tailHeader+ev("a"), true, false)      // 24 h due: part 1
	step(ev("b"), true, false)                 // 24 h due again: part 2
	step(ev("c1")+ev("c2"), false, false)      // a part's worth: part 3
	step(`[9.5,"o","no newline"`, false, true) // exit: part 4

	want := []upload{
		{"/rec", tailHeader + ev("a")},
		{"/rec/parts/2", tailHeader + ev("b")},
		{"/rec/parts/3", tailHeader + ev("c1") + ev("c2")},
		{"/rec/parts/4", tailHeader + `[9.5,"o","no newline"`},
	}
	ups := ps.uploads()
	if len(ups) != len(want) {
		t.Fatalf("parts = %+v, want %+v", ups, want)
	}
	for i := range want {
		if ups[i] != want[i] {
			t.Errorf("part %d = %+v, want %+v", i+1, ups[i], want[i])
		}
	}
	file, _ := os.ReadFile(cast)
	if got := joinParts(t, ups); got != string(file) {
		t.Fatalf("joined parts =\n%q\nwant the cast file\n%q", got, file)
	}
}

// faultServer stores parts by path, replacing on a repeat like the control
// plane's upsert, and fails the FIRST attempt at each path in cut: "body"
// drops the connection halfway through the body (nothing stored), "reply"
// stores the part and then drops the connection before answering.
type faultServer struct {
	mu       sync.Mutex
	stored   map[string]string
	attempts map[string]int
	cut      map[string]string
	srv      *httptest.Server
}

func newFaultServer(t *testing.T, cut map[string]string) *faultServer {
	fs := &faultServer{stored: map[string]string{}, attempts: map[string]int{}, cut: cut}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.attempts[r.URL.Path]++
		mode := ""
		if fs.attempts[r.URL.Path] == 1 {
			mode = fs.cut[r.URL.Path]
		}
		fs.mu.Unlock()
		if mode == "body" {
			_, _ = io.ReadFull(r.Body, make([]byte, r.ContentLength/2))
			dropConn(t, w)
			return
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		fs.mu.Lock()
		fs.stored[r.URL.Path] = string(b)
		fs.mu.Unlock()
		if mode == "reply" {
			dropConn(t, w)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

func dropConn(t *testing.T, w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	_ = conn.Close()
}

// joinStored is joinParts over what the server holds: part 1, then part n for
// as long as there is one.
func (fs *faultServer) joinStored(t *testing.T) string {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	ups := []upload{{"/rec", fs.stored["/rec"]}}
	for n := 2; ; n++ {
		p := "/rec/parts/" + strconv.Itoa(n)
		b, ok := fs.stored[p]
		if !ok {
			break
		}
		ups = append(ups, upload{p, b})
	}
	return joinParts(t, ups)
}

// TestTailUploader_AnUploadCutMidwayIsResentWithoutLossOrDuplication: a part
// whose connection drops mid-body, and one the server stored but whose answer
// never came back, are each sent again under the same part number, from the
// same cast offset, once the retry wait is over, and the cast grows meanwhile.
// The stored parts join back to the cast file exactly: no event lost, none twice.
func TestTailUploader_AnUploadCutMidwayIsResentWithoutLossOrDuplication(t *testing.T) {
	ev := func(i int) string { return `[` + strconv.Itoa(i) + `.5,"o","line ` + strconv.Itoa(i) + `"]` + "\n" }
	shrinkParts(t, int64(len(tailHeader)+2*len(ev(0))))
	fs := newFaultServer(t, map[string]string{"/rec/parts/2": "body", "/rec/parts/3": "reply"})
	cast := filepath.Join(t.TempDir(), "r.cast")
	tu := &tailUploader{cast: cast, url: fs.srv.URL + "/rec", part: 1, due: time.Now().Add(time.Hour)}
	i := 0
	grow := func(n int) {
		var b strings.Builder
		for range n {
			b.WriteString(ev(i))
			i++
		}
		writeCast(t, cast, b.String())
	}
	writeCast(t, cast, tailHeader)
	grow(6)
	if err := tu.flush(false); err == nil || tu.part != 2 {
		t.Fatalf("a part cut mid-body: err=%v next part %d, want an error and part 2 still next", err, tu.part)
	}
	for range 2 {
		grow(2)
		tu.retryAt = time.Time{}
		_ = tu.flush(false)
	}
	grow(1)
	if err := tu.flush(true); err != nil {
		t.Fatal(err)
	}

	fs.mu.Lock()
	a2, a3 := fs.attempts["/rec/parts/2"], fs.attempts["/rec/parts/3"]
	fs.mu.Unlock()
	if a2 != 2 || a3 != 2 {
		t.Errorf("attempts: part 2 = %d, part 3 = %d; want each sent twice", a2, a3)
	}
	file, _ := os.ReadFile(cast)
	if got := fs.joinStored(t); got != string(file) {
		t.Fatalf("stored parts join to\n%q\nwant the cast file\n%q", got, file)
	}
}

// TestTailUploader_IntervalPartStopsAtTheLastWholeLine: a part the 24 h
// interval sends ends at the last whole line; a line still being written
// waits for the next part, so no event is split between two masked uploads.
func TestTailUploader_IntervalPartStopsAtTheLastWholeLine(t *testing.T) {
	ps := newPartServer(t)
	cast := filepath.Join(t.TempDir(), "r.cast")
	writeCast(t, cast, tailHeader+eventLines(2)+`[7.5,"o","half a li`)
	tu := &tailUploader{cast: cast, url: ps.srv.URL + "/rec", part: 1, due: time.Now()}
	if err := tu.flush(false); err != nil {
		t.Fatal(err)
	}
	writeCast(t, cast, `ne"]`+"\n")
	if err := tu.flush(true); err != nil {
		t.Fatal(err)
	}
	want := []upload{{"/rec", tailHeader + eventLines(2)}, {"/rec/parts/2", tailHeader + `[7.5,"o","half a line"]` + "\n"}}
	if ups := ps.uploads(); len(ups) != 2 || ups[0] != want[0] || ups[1] != want[1] {
		t.Fatalf("parts = %+v, want %+v", ups, want)
	}
}
