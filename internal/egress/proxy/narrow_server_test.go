// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const narrowTestBase = `{"run_id":"r","policy":{"allowed_domains":["tls.test","keep.test"]},"listen":":3128"}`

// narrowEnv is one proxy with its narrowing socket, a control plane that records the reports it gets,
// and counters for what the socket's handler did.
type narrowEnv struct {
	t       *testing.T
	dir     string
	p       *Proxy
	srv     *Server
	cp      *httptest.Server
	base    []byte
	reports chan LoadedReport
	built   atomic.Int32 // loaded reports the socket asked to build
	// onReport, when set, decides a report's response status (200 when it answers 0).
	onReport func(r *http.Request, rep LoadedReport) int
}

// shortDir is a directory whose path leaves room for a unix socket's.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newNarrowEnv(t *testing.T, dir string) *narrowEnv {
	t.Helper()
	shortReportBackoff(t)
	e := &narrowEnv{t: t, dir: dir, base: []byte(narrowTestBase), reports: make(chan LoadedReport, 64)}
	e.cp = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep LoadedReport
		_ = json.NewDecoder(r.Body).Decode(&rep)
		status := http.StatusOK
		if e.onReport != nil {
			if s := e.onReport(r, rep); s != 0 {
				status = s
			}
		}
		if status/100 == 2 {
			e.reports <- rep
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(e.cp.Close)
	e.p = e.newProxy()
	e.srv = &Server{proxy: e.p}
	return e
}

// newProxy is a proxy for the test policy: its control-plane calls reach e.cp and every other dial an echo server.
func (e *narrowEnv) newProxy() *Proxy {
	cpDial, echoDial := redirectDial(upstreamAddr(e.cp)), redirectDial(startEcho(e.t))
	return newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"tls.test", "keep.test"}}),
		Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
		Resolver: publicResolver{},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasSuffix(addr, ":8080") {
				return cpDial(ctx, network, addr)
			}
			return echoDial(ctx, network, addr)
		},
		ControlPlaneURL: "https://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		ControlTLS:      testInsecureTLSConfig,
	})
}

// serve starts the narrowing socket on ctx, over the list file already in the directory.
func (e *narrowEnv) serve(ctx context.Context) {
	e.t.Helper()
	if list, err := ReadLiveDenyFile(e.dir); err != nil {
		e.t.Fatal(err)
	} else {
		e.p.policy.live.set(list)
	}
	err := e.srv.ServeNarrow(ctx, e.dir, e.base, func(b []byte) LoadedReport {
		e.built.Add(1)
		return LoadedReport{ConfigSHA256: ConfigSHA256(b), ProxyVersion: "test"}
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *narrowEnv) start() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	e.serve(ctx)
	return ctx
}

func narrowBody(hosts ...string) []byte {
	b, _ := json.Marshal(map[string][]string{"deny": append([]string{}, hosts...)})
	return b
}

// ask sends body to the socket and reads the answer to its end. ok is false when none came.
func ask(t *testing.T, dir string, body []byte) (ans NarrowAnswer, line string, ok bool) {
	t.Helper()
	c, err := net.Dial("unix", NarrowSocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.UnixConn).CloseWrite()
	raw, _ := io.ReadAll(io.LimitReader(c, MaxNarrowMessageBytes+1))
	if len(bytes.TrimSpace(raw)) == 0 {
		return NarrowAnswer{}, "", false
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		t.Fatalf("answer %q: %v", raw, err)
	}
	return ans, string(raw), true
}

func (e *narrowEnv) mustAsk(body []byte) NarrowAnswer {
	e.t.Helper()
	ans, _, ok := ask(e.t, e.dir, body)
	if !ok {
		e.t.Fatalf("no answer to %s", body)
	}
	return ans
}

func (e *narrowEnv) denied(host string) bool { return e.p.policy.evalHost(host, 443) == hostDeny }

func (e *narrowEnv) fileList() ([]string, bool) {
	b, err := os.ReadFile(filepath.Join(e.dir, liveDenyFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	var l []string
	if err := json.Unmarshal(b, &l); err != nil {
		e.t.Fatalf("list file %q: %v", b, err)
	}
	return l, true
}

func (e *narrowEnv) wantReport(hash string) {
	e.t.Helper()
	select {
	case r := <-e.reports:
		if r.ConfigSHA256 != hash {
			e.t.Fatalf("reported %s, want %s", r.ConfigSHA256, hash)
		}
	case <-time.After(10 * time.Second):
		e.t.Fatal("no loaded report arrived")
	}
}

func (e *narrowEnv) held() int {
	e.srv.live.mu.Lock()
	defer e.srv.live.mu.Unlock()
	return len(e.srv.live.conns)
}

func (e *narrowEnv) effHash(list ...string) string {
	e.t.Helper()
	eff, err := WithLiveDenies(e.base, list)
	if err != nil {
		e.t.Fatal(err)
	}
	return ConfigSHA256(eff)
}

// front serves e.p on a tracked listener and answers its address.
func (e *narrowEnv) front() string {
	e.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	hs := &http.Server{Handler: e.p}
	go hs.Serve(&trackedListener{Listener: ln, set: &e.srv.live})
	e.t.Cleanup(func() { hs.Close() })
	return ln.Addr().String()
}

// failDirSync makes fsyncDir fail while fail is set, and counts its calls.
func failDirSync(t *testing.T, fail *atomic.Bool) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	old := fsyncDir
	fsyncDir = func(dir string) error {
		calls.Add(1)
		if fail.Load() {
			return errors.New("injected directory sync failure")
		}
		return old(dir)
	}
	t.Cleanup(func() { fsyncDir = old })
	return &calls
}

func TestNarrow_ProbeWritesNothing(t *testing.T) {
	e := newNarrowEnv(t, shortDir(t))
	var syncs atomic.Int32
	old := fsyncDir
	fsyncDir = func(dir string) error { syncs.Add(1); return old(dir) }
	t.Cleanup(func() { fsyncDir = old })
	// A stale socket file from a crashed proxy does not stop the listener.
	if err := os.WriteFile(NarrowSocketPath(e.dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.start()
	addr := e.front()
	conn, status := connectThrough(t, "http://"+addr, "tls.test:443")
	defer conn.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT = %q", status)
	}
	ans := e.mustAsk(narrowBody())
	if ans.State != "current" || ans.Changed || len(ans.LiveDenies) != 0 || ans.ConfigSHA256 == "" || ans.BaseSHA256 == "" {
		t.Fatalf("probe answer %+v", ans)
	}
	if ans.ConfigSHA256 != ConfigSHA256(e.base) {
		t.Errorf("probe hash %s is not the base's %s", ans.ConfigSHA256, ConfigSHA256(e.base))
	}
	names := dirNames(t, e.dir)
	if !slices.Equal(names, []string{narrowSocket}) {
		t.Errorf("directory holds %v after a probe, want only the socket", names)
	}
	if syncs.Load() != 0 || e.built.Load() != 0 {
		t.Errorf("a probe synced %d times and built %d reports", syncs.Load(), e.built.Load())
	}
	if e.held() != 1 {
		t.Errorf("a probe closed a held connection: %d held", e.held())
	}
	if fi, err := os.Stat(NarrowSocketPath(e.dir)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket %v, %v; want mode 0600", fi, err)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, en := range ents {
		names = append(names, en.Name())
	}
	return names
}

func TestNarrow_AppliesPersistsReportsCloses(t *testing.T) {
	e := newNarrowEnv(t, shortDir(t))
	e.start()
	addr := e.front()
	conn, status := connectThrough(t, "http://"+addr, "tls.test:443")
	defer conn.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT = %q", status)
	}
	if e.denied("tls.test") {
		t.Fatal("denied before the request")
	}
	ans := e.mustAsk(narrowBody(" TLS.test "))
	want := e.effHash("tls.test")
	if ans.State != "applied" || !ans.Changed || ans.ConfigSHA256 != want || !slices.Equal(ans.LiveDenies, []string{"tls.test"}) || ans.Error != "" {
		t.Fatalf("answer %+v; want applied, changed, hash %s", ans, want)
	}
	if wantBase, _ := LiveDenyBaseSHA256(e.base); ans.BaseSHA256 != wantBase {
		t.Errorf("base hash %s, want %s", ans.BaseSHA256, wantBase)
	}
	if !e.denied("tls.test") || e.denied("keep.test") {
		t.Error("the verdicts did not change as asked")
	}
	if l, ok := e.fileList(); !ok || !slices.Equal(l, []string{"tls.test"}) {
		t.Errorf("file list %v, %v", l, ok)
	}
	if fi, err := os.Stat(filepath.Join(e.dir, liveDenyFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("list file %v, %v; want mode 0600", fi, err)
	}
	if e.held() != 0 {
		t.Errorf("%d client connections survived", e.held())
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("the held tunnel still reads")
	}
	e.wantReport(want)
	if e.built.Load() != 1 {
		t.Errorf("%d reports built, want 1", e.built.Load())
	}
	for _, n := range dirNames(t, e.dir) {
		if strings.HasSuffix(n, ".tmp") {
			t.Errorf("tmp file %s left behind", n)
		}
	}
}

func TestNarrow_NoChangeStillReports(t *testing.T) {
	e := newNarrowEnv(t, shortDir(t))
	var fail atomic.Bool
	syncs := failDirSync(t, &fail)
	e.start()
	first := e.mustAsk(narrowBody("tls.test"))
	e.wantReport(first.ConfigSHA256)
	fi1, err := os.Stat(filepath.Join(e.dir, liveDenyFile))
	if err != nil {
		t.Fatal(err)
	}
	addr := e.front()
	conn, _ := connectThrough(t, "http://"+addr, "keep.test:443")
	defer conn.Close()
	again := e.mustAsk(narrowBody("tls.test"))
	if again.State != "applied" || again.Changed || again.ConfigSHA256 != first.ConfigSHA256 {
		t.Fatalf("same-list answer %+v after %+v", again, first)
	}
	e.wantReport(first.ConfigSHA256)
	fi2, err := os.Stat(filepath.Join(e.dir, liveDenyFile))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(fi1, fi2) {
		t.Error("the list file was not written again")
	}
	if syncs.Load() != 2 || e.built.Load() != 2 {
		t.Errorf("%d directory syncs and %d reports built, want 2 and 2", syncs.Load(), e.built.Load())
	}
	if e.held() != 1 {
		t.Errorf("a no-op request changed the held connections: %d", e.held())
	}
}

func TestNarrow_PersistFailureChangesNothing(t *testing.T) {
	e := newNarrowEnv(t, shortDir(t))
	e.start()
	addr := e.front()
	conn, _ := connectThrough(t, "http://"+addr, "tls.test:443")
	defer conn.Close()
	// A non-empty directory where the tmp file goes: it can be neither opened nor removed, even by root.
	block := filepath.Join(e.dir, liveDenyFile+".tmp")
	if err := os.MkdirAll(filepath.Join(block, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	ans := e.mustAsk(narrowBody("tls.test"))
	if ans.State != "refused" || ans.Changed || ans.Error == "" || len(ans.LiveDenies) != 0 {
		t.Fatalf("answer %+v; want refused with the unchanged list", ans)
	}
	if e.denied("tls.test") || e.held() != 1 || e.built.Load() != 0 {
		t.Errorf("a refused write changed something: denied=%v held=%d built=%d", e.denied("tls.test"), e.held(), e.built.Load())
	}
	if _, ok := e.fileList(); ok {
		t.Error("a list file exists")
	}
	if err := os.RemoveAll(block); err != nil {
		t.Fatal(err)
	}
	if ans := e.mustAsk(narrowBody("tls.test")); ans.State != "applied" {
		t.Fatalf("after the block was removed: %+v", ans)
	}
}

func TestNarrow_OversizedOrBadRequestRefused(t *testing.T) {
	e := newNarrowEnv(t, shortDir(t))
	e.start()
	pad := func(obj string, n int) []byte { return append([]byte(obj), bytes.Repeat([]byte(" "), n-len(obj))...) }
	for name, body := range map[string][]byte{
		"one byte over the bound": pad(`{"deny":["tls.test"]}`, MaxNarrowMessageBytes+1),
		"unknown field":           []byte(`{"deny":["tls.test"],"also":1}`),
		"not json":                []byte(`deny tls.test`),
		"no deny list":            []byte(`{}`),
		"data after the object":   []byte(`{"deny":["tls.test"]} {"deny":[]}`),
		"invalid entry":           narrowBody("tls.test", "bad host"),
		"empty entry":             narrowBody(""),
		"too many":                narrowBody(manyHosts(MaxLiveDenies + 1)...),
	} {
		ans, line, ok := ask(t, e.dir, body)
		if !ok || ans.State != "refused" || ans.Changed || ans.Error == "" || !strings.Contains(line, `"live_denies":[]`) {
			t.Errorf("%s: answer %q (ok=%v); want refused with an empty list", name, line, ok)
		}
	}
	if e.denied("tls.test") || e.built.Load() != 0 {
		t.Error("a refused request changed the verdicts or built a report")
	}
	if names := dirNames(t, e.dir); !slices.Equal(names, []string{narrowSocket}) {
		t.Errorf("directory holds %v, want only the socket", names)
	}
	// Exactly the bound is read.
	if ans, _, ok := ask(t, e.dir, pad(`{"deny":[]}`, MaxNarrowMessageBytes)); !ok || ans.State != "current" {
		t.Errorf("a request of exactly the bound: %+v, %v", ans, ok)
	}
}

func manyHosts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("h%d.example", i)
	}
	return out
}

func TestNarrow_DirFsyncFailureAnswersNothing(t *testing.T) {
	e := newNarrowEnv(t, shortDir(t))
	var fail atomic.Bool
	fail.Store(true)
	syncs := failDirSync(t, &fail)
	e.start()
	addr := e.front()
	conn, _ := connectThrough(t, "http://"+addr, "tls.test:443")
	defer conn.Close()
	if _, _, ok := ask(t, e.dir, narrowBody("tls.test")); ok {
		t.Fatal("an answer came for a list whose directory sync failed")
	}
	if syncs.Load() != 1 || e.built.Load() != 0 {
		t.Errorf("%d syncs and %d reports built, want 1 and 0", syncs.Load(), e.built.Load())
	}
	if !e.denied("tls.test") || e.held() != 0 {
		t.Errorf("denied=%v held=%d; the proxy must be at least as narrow as the file may be", e.denied("tls.test"), e.held())
	}
	if l, ok := e.fileList(); !ok || !slices.Equal(l, []string{"tls.test"}) {
		t.Errorf("file list %v, %v", l, ok)
	}
	select {
	case r := <-e.reports:
		t.Errorf("a report was sent: %+v", r)
	default:
	}
}

// A narrowing whose directory sync fails is never answered; wardynd, gone before its answer, probes and sends
// the same list again. That request writes the list afresh and answers only once its own sync succeeds, with
// or without a proxy restart in between.
func TestNarrow_FailedDirSyncThenSameListRetry(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			e := newNarrowEnv(t, shortDir(t))
			var fail atomic.Bool
			syncs := failDirSync(t, &fail)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.serve(ctx)
			inode := func() os.FileInfo {
				fi, err := os.Stat(filepath.Join(e.dir, liveDenyFile))
				if err != nil {
					t.Fatal(err)
				}
				return fi
			}
			noTmp := func() {
				for _, n := range dirNames(t, e.dir) {
					if strings.HasSuffix(n, ".tmp") {
						t.Errorf("tmp file %s left behind", n)
					}
				}
			}

			// 1. The narrowing's directory sync fails: one sync call, no answer, no report, the list in memory and in the file.
			fail.Store(true)
			if _, _, ok := ask(t, e.dir, narrowBody("d.example")); ok {
				t.Fatal("step 1: an answer came")
			}
			if syncs.Load() != 1 || e.built.Load() != 0 || !e.denied("d.example") {
				t.Fatalf("step 1: syncs=%d built=%d denied=%v; want 1, 0, true", syncs.Load(), e.built.Load(), e.denied("d.example"))
			}
			written := inode()

			// 2. wardynd dies before its answer: nothing more is sent, the proxy runs on (or restarts on the same directory).
			if restart {
				cancel()
				waitSocketGone(t, e.dir)
				e = &narrowEnv{t: t, dir: e.dir, base: e.base, reports: e.reports, cp: e.cp}
				e.p = e.newProxy()
				e.srv = &Server{proxy: e.p}
				ctx2, cancel2 := context.WithCancel(context.Background())
				defer cancel2()
				e.serve(ctx2)
				if !e.denied("d.example") {
					t.Fatal("step 2: the restarted proxy does not enforce the file")
				}
				written = inode()
			}

			// 3. The recovery probe sees the list and writes nothing.
			before := syncs.Load()
			probe := e.mustAsk(narrowBody())
			if probe.State != "current" || probe.Changed || !slices.Equal(probe.LiveDenies, []string{"d.example"}) || probe.ConfigSHA256 != e.effHash("d.example") {
				t.Fatalf("step 3: probe %+v", probe)
			}
			if syncs.Load() != before || !os.SameFile(written, inode()) {
				t.Fatal("step 3: the probe wrote or synced")
			}

			// 4. The same list while the sync still fails: written again (a new file, a sync call), no answer, no report.
			if _, _, ok := ask(t, e.dir, narrowBody("d.example")); ok {
				t.Fatal("step 4: an answer came while the directory sync fails")
			}
			if syncs.Load() != before+1 || os.SameFile(written, inode()) || e.built.Load() != 0 {
				t.Fatalf("step 4: syncs=%d (want %d) rewritten=%v built=%d", syncs.Load(), before+1, !os.SameFile(written, inode()), e.built.Load())
			}
			written = inode()

			// 5. The sync works: a fresh write and sync before the applied answer and its report.
			fail.Store(false)
			ans := e.mustAsk(narrowBody("d.example"))
			if ans.State != "applied" || ans.Changed || ans.ConfigSHA256 != e.effHash("d.example") {
				t.Fatalf("step 5: answer %+v", ans)
			}
			if syncs.Load() != before+2 || os.SameFile(written, inode()) {
				t.Errorf("step 5: syncs=%d (want %d) rewritten=%v", syncs.Load(), before+2, !os.SameFile(written, inode()))
			}
			if l, _ := e.fileList(); !slices.Equal(l, []string{"d.example"}) {
				t.Errorf("step 5: file list %v", l)
			}
			e.wantReport(ans.ConfigSHA256)
			noTmp()
		})
	}
}

// waitSocketGone returns once the old listener has closed and unlinked its socket.
func waitSocketGone(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.Dial("unix", NarrowSocketPath(dir))
		if err != nil {
			return
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the old socket never closed")
		}
	}
}

func TestNarrow_DelayedOlderRequestNeverWidens(t *testing.T) {
	e := newNarrowEnv(t, shortDir(t))
	e.start()
	if ans := e.mustAsk(narrowBody("a.example", "b.example")); !slices.Equal(ans.LiveDenies, []string{"a.example", "b.example"}) {
		t.Fatalf("first answer %+v", ans)
	}
	late := e.mustAsk(narrowBody("a.example"))
	if late.State != "applied" || late.Changed || !slices.Equal(late.LiveDenies, []string{"a.example", "b.example"}) {
		t.Fatalf("late answer %+v; want the list kept at {a,b}", late)
	}
	if !e.denied("b.example") {
		t.Error("b.example was let go")
	}
	if l, _ := e.fileList(); !slices.Equal(l, []string{"a.example", "b.example"}) {
		t.Errorf("file list %v", l)
	}
}

func TestNarrow_SerializesConcurrentRequests(t *testing.T) {
	e := newNarrowEnv(t, shortDir(t))
	e.start()
	const n = 24
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		lens []int
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ans, _, ok := ask(t, e.dir, narrowBody(fmt.Sprintf("h%d.example", i)))
			if !ok || ans.State != "applied" || !slices.Contains(ans.LiveDenies, fmt.Sprintf("h%d.example", i)) {
				t.Errorf("request %d: %+v ok=%v", i, ans, ok)
			}
			mu.Lock()
			lens = append(lens, len(ans.LiveDenies))
			mu.Unlock()
		}()
	}
	wg.Wait()
	slices.Sort(lens)
	for i, l := range lens {
		if l != i+1 {
			t.Fatalf("answer list lengths %v: requests did not run one at a time", lens)
		}
	}
	if l, _ := e.fileList(); len(l) != n {
		t.Errorf("file holds %d hosts, want %d", len(l), n)
	}
	for i := range n {
		if !e.denied(fmt.Sprintf("h%d.example", i)) {
			t.Errorf("h%d.example not denied", i)
		}
	}
}

func TestNarrow_ClosesHeldConnections(t *testing.T) {
	e := newNarrowEnv(t, shortDir(t))
	e.start()
	addr := e.front()
	tunnel, status := connectThrough(t, "http://"+addr, "tls.test:443")
	defer tunnel.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT = %q", status)
	}
	// A keep-alive connection: a request answered 403, the connection left open.
	tr := &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: addr})}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Get("http://nothere.test/")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || e.held() != 2 {
		t.Fatalf("status %d with %d connections held, want 403 and 2", resp.StatusCode, e.held())
	}
	if ans := e.mustAsk(narrowBody("tls.test")); ans.State != "applied" || !ans.Changed {
		t.Fatalf("answer %+v", ans)
	}
	if e.held() != 0 {
		t.Errorf("%d connections survived", e.held())
	}
	_ = tunnel.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := tunnel.Read(make([]byte, 1)); err == nil {
		t.Error("the tunnel still reads")
	}
	// A connection accepted after the swap is judged under the new list.
	late, status := connectThrough(t, "http://"+addr, "tls.test:443")
	defer late.Close()
	if !strings.Contains(status, "403") {
		t.Errorf("CONNECT after the swap = %q, want 403", status)
	}
	other, status := connectThrough(t, "http://"+addr, "keep.test:443")
	defer other.Close()
	if !strings.Contains(status, "200") {
		t.Errorf("CONNECT to a host not denied = %q, want 200", status)
	}
}

func TestNarrow_CancelsPreviousReportLoop(t *testing.T) {
	e := newNarrowEnv(t, shortDir(t))
	bootHash := ConfigSHA256(e.base)
	loadedReportAttempt = time.Minute // a report ends only by being cancelled, never by its own timeout
	inFlight := make(chan struct{}, 8)
	cancelled := make(chan struct{}, 8)
	e.onReport = func(r *http.Request, rep LoadedReport) int {
		if rep.ConfigSHA256 != bootHash {
			return 0
		}
		inFlight <- struct{}{}
		<-r.Context().Done() // the boot report never lands: its loop only ends by being cancelled
		cancelled <- struct{}{}
		return http.StatusServiceUnavailable
	}
	ctx := e.start()
	e.srv.StartLoadedReport(ctx, LoadedReport{ConfigSHA256: bootHash, ProxyVersion: "test"})
	select {
	case <-inFlight:
	case <-time.After(10 * time.Second):
		t.Fatal("the boot report was never sent")
	}
	ans := e.mustAsk(narrowBody("tls.test"))
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the boot report loop was not cancelled by the narrowing's report")
	}
	e.wantReport(ans.ConfigSHA256)
	// A second narrowing replaces the first's loop in turn, and stops the one before.
	e.onReport = nil
	ans2 := e.mustAsk(narrowBody("keep.test"))
	e.wantReport(ans2.ConfigSHA256)
}

// spyConn records the calls that reach the connection it wraps.
type spyConn struct {
	net.Conn
	mu     sync.Mutex
	events []string
}

func (c *spyConn) record(ev string)  { c.mu.Lock(); c.events = append(c.events, ev); c.mu.Unlock() }
func (c *spyConn) CloseWrite() error { c.record("closewrite"); return nil }
func (c *spyConn) Close() error      { c.record("close"); return c.Conn.Close() }

type spyListener struct {
	net.Listener
	conns chan net.Conn
}

func (l spyListener) Accept() (net.Conn, error) { return <-l.conns, nil }

func TestTrackedConn_ForwardsCloseWrite(t *testing.T) {
	a1, a2 := net.Pipe()
	defer a2.Close()
	spy := &spyConn{Conn: a1}
	conns := make(chan net.Conn, 1)
	conns <- spy
	var live liveState
	tl := &trackedListener{Listener: spyListener{conns: conns}, set: &live}
	tracked, err := tl.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if len(live.conns) != 1 {
		t.Fatalf("%d connections tracked after Accept, want 1", len(live.conns))
	}
	// tunnel half-closes its destination through CloseWrite when the other side ends.
	b1, b2 := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); tunnel(tracked, b1) }()
	b2.Close() // the upstream ends: tunnel must half-close the tracked client side first
	<-done
	spy.mu.Lock()
	events := slices.Clone(spy.events)
	spy.mu.Unlock()
	if !slices.Equal(events, []string{"closewrite", "close"}) {
		t.Fatalf("calls on the wrapped connection: %v; want a half-close, then the close", events)
	}
	if len(live.conns) != 0 {
		t.Errorf("%d connections still tracked after Close", len(live.conns))
	}
	// A second Close is harmless.
	_ = tracked.Close()
}

// Server.listen wraps every listener it returns, with or without allowed peers.
func TestServerListen_TracksEveryListener(t *testing.T) {
	for _, peers := range [][]string{nil, {"127.0.0.0/8"}} {
		s := &Server{}
		for _, p := range peers {
			s.allowedPeers = append(s.allowedPeers, netip.MustParsePrefix(p))
		}
		ln, err := s.listen("127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		tl, ok := ln.(*trackedListener)
		if !ok {
			t.Fatalf("listen returned %T with peers %v", ln, peers)
		}
		go func() {
			if c, err := net.Dial("tcp", ln.Addr().String()); err == nil {
				defer c.Close()
				_, _ = c.Read(make([]byte, 1))
			}
		}()
		c, err := tl.Accept()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := c.(*trackedConn); !ok || len(s.live.conns) != 1 {
			t.Errorf("accepted %T, %d tracked", c, len(s.live.conns))
		}
		s.live.closeAll()
		ln.Close()
	}
}
