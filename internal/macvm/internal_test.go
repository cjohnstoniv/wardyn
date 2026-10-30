// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

func testSpec(t *testing.T) MachineSpec {
	t.Helper()
	g, h, err := NewNetPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = h.Close() })
	dir := t.TempDir()
	return MachineSpec{
		CPUs: 4, MemoryBytes: 8 << 30, MinCPUs: 2, MinMemoryBytes: 4 << 30,
		HardwareModel: []byte("hw"), MachineIdentifier: []byte("id"),
		DiskPath: filepath.Join(dir, "run", DiskFile), AuxPath: filepath.Join(dir, "run", AuxFile),
		BaseDir: filepath.Join(dir, "base"), Net: g,
	}
}

// The plan has exactly one NIC, a FileHandle attachment, and the fixed display.
func TestBuildPlanOneFileHandleNICAndDisplay(t *testing.T) {
	s := testSpec(t)
	p, err := BuildPlan(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Network) != 1 || p.Network[0].Kind != NetworkFileHandle || p.Network[0].File != s.Net || p.Network[0].MTU != 1500 {
		t.Fatalf("network = %+v", p.Network)
	}
	if len(p.Displays) != 1 || p.Displays[0] != (Display{Width: 1920, Height: 1080, PPI: 80}) {
		t.Fatalf("displays = %+v", p.Displays)
	}
	if len(p.Storage) != 1 || p.Storage[0].ReadOnly || p.Storage[0].Path != s.DiskPath {
		t.Fatalf("storage = %+v", p.Storage)
	}
	if p.CPUs != 4 || p.MemoryBytes != 8<<30 {
		t.Fatalf("size raised or changed: %d CPUs, %d bytes", p.CPUs, p.MemoryBytes)
	}
	if err := CheckPlan(p); err != nil {
		t.Fatal(err)
	}
	two := p
	two.Network = append(append([]NetworkDevice(nil), p.Network...), p.Network[0])
	if CheckPlan(two) == nil {
		t.Fatal("CheckPlan accepted two NICs")
	}
	other := p
	other.Network = []NetworkDevice{{Kind: "nat", File: s.Net}}
	if CheckPlan(other) == nil {
		t.Fatal("CheckPlan accepted a non-FileHandle NIC")
	}
}

func TestBuildPlanRefusals(t *testing.T) {
	for name, mod := range map[string]func(*MachineSpec){
		"below min CPUs":    func(s *MachineSpec) { s.CPUs = 1 },
		"below min memory":  func(s *MachineSpec) { s.MemoryBytes = 2 << 30 },
		"odd memory":        func(s *MachineSpec) { s.MemoryBytes = 8<<30 + 1 },
		"no NIC":            func(s *MachineSpec) { s.Net = nil },
		"disk in the base":  func(s *MachineSpec) { s.DiskPath = filepath.Join(s.BaseDir, DiskFile) },
		"aux in the base":   func(s *MachineSpec) { s.AuxPath = filepath.Join(s.BaseDir, "sub", "..", AuxFile) },
		"no machine id":     func(s *MachineSpec) { s.MachineIdentifier = nil },
		"no hardware model": func(s *MachineSpec) { s.HardwareModel = nil },
	} {
		s := testSpec(t)
		mod(&s)
		if _, err := BuildPlan(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	s := testSpec(t)
	s.DiskPath = s.BaseDir + "-sibling/disk.img" // not inside
	if _, err := BuildPlan(s); err != nil {
		t.Fatalf("sibling of the base refused: %v", err)
	}
}

func dockerChunk(stream byte, b []byte) []byte {
	h := make([]byte, 8, 8+len(b))
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(b)))
	return append(h, b...)
}

func lenFrame(b []byte) []byte {
	out := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	return append(out, b...)
}

func frame(n int, fill byte) []byte { return bytes.Repeat([]byte{fill}, n) }

// The bridge frames guest datagrams onto stdin and turns Docker's
// multiplexed stdout back into datagrams, sending stderr to the log.
func TestBridgeBothWays(t *testing.T) {
	guestVM, hostNIC, err := NewNetPair()
	if err != nil {
		t.Fatal(err)
	}
	defer guestVM.Close()
	defer hostNIC.Close()
	relaySide, attach := net.Pipe()
	defer relaySide.Close()
	var stderr bytes.Buffer
	b := &Bridge{Guest: hostNIC, Attach: attach, Stderr: &stderr}
	errc := make(chan error, 1)
	go func() { errc <- b.Run() }()

	// guest -> relay
	f1 := frame(60, 0xAB)
	if _, err := guestVM.Write(f1); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4+len(f1))
	if _, err := io.ReadFull(relaySide, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, lenFrame(f1)) {
		t.Fatalf("relay got %x", got[:8])
	}

	// relay -> guest, split across chunks, with a stderr chunk between.
	f2 := frame(1500, 0xCD)
	wire := lenFrame(f2)
	go func() {
		_, _ = relaySide.Write(dockerChunk(streamStdout, wire[:10]))
		_, _ = relaySide.Write(dockerChunk(streamStderr, []byte("relay says hi")))
		_, _ = relaySide.Write(dockerChunk(streamStdout, wire[10:]))
	}()
	buf := make([]byte, maxNetFrame)
	_ = guestVM.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := guestVM.Read(buf)
	if err != nil || !bytes.Equal(buf[:n], f2) {
		t.Fatalf("guest got %d bytes, %v", n, err)
	}

	// An out-of-bounds length from the relay ends the bridge.
	go func() { _, _ = relaySide.Write(dockerChunk(streamStdout, []byte{0, 0, 0, 3})) }()
	select {
	case err := <-errc:
		if !errors.Is(err, errNetFrame) {
			t.Fatalf("bridge ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not end on a bad length")
	}
	if !strings.Contains(stderr.String(), "relay says hi") {
		t.Fatalf("stderr not passed on: %q", stderr.String())
	}
}

// fullGuest is a guest NIC whose receive buffer is full for its first
// write (ENOBUFS, as macOS returns), then takes frames.
type fullGuest struct {
	writes int
	got    chan []byte
	stop   chan struct{}
}

func (g *fullGuest) Read([]byte) (int, error) { <-g.stop; return 0, io.EOF }

func (g *fullGuest) Write(p []byte) (int, error) {
	if g.writes++; g.writes == 1 {
		return 0, &os.PathError{Op: "write", Path: "guest-nic-host", Err: unix.ENOBUFS}
	}
	g.got <- append([]byte(nil), p...)
	return len(p), nil
}

// A frame the guest's full receive buffer refuses is dropped; the bridge
// goes on and the next frame reaches the guest.
func TestBridgeDropsOnFullGuestBuffer(t *testing.T) {
	g := &fullGuest{got: make(chan []byte, 1), stop: make(chan struct{})}
	defer close(g.stop)
	relaySide, attach := net.Pipe()
	defer relaySide.Close()
	b := &Bridge{Guest: g, Attach: attach}
	errc := make(chan error, 1)
	go func() { errc <- b.Run() }()
	f1, f2 := frame(60, 0x01), frame(60, 0x02)
	go func() {
		_, _ = relaySide.Write(dockerChunk(streamStdout, lenFrame(f1)))
		_, _ = relaySide.Write(dockerChunk(streamStdout, lenFrame(f2)))
	}()
	select {
	case got := <-g.got:
		if !bytes.Equal(got, f2) || b.dropped.Load() != 1 {
			t.Fatalf("guest got %x, dropped %d", got[:1], b.dropped.Load())
		}
	case err := <-errc:
		t.Fatalf("the bridge ended: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no frame reached the guest")
	}
}

func TestDockerDemuxRefusesUnknownStream(t *testing.T) {
	d := &dockerDemux{r: bytes.NewReader(dockerChunk(7, []byte("x")))}
	if _, err := d.Read(make([]byte, 4)); err == nil {
		t.Fatal("read an unknown stream type")
	}
	big := make([]byte, 8)
	big[0] = streamStdout
	binary.BigEndian.PutUint32(big[4:], maxStreamChunk+1)
	d = &dockerDemux{r: bytes.NewReader(big)}
	if _, err := d.Read(make([]byte, 4)); err == nil {
		t.Fatal("accepted an oversize chunk")
	}
}

func TestRotatingLogCapsAtTwoFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), HelperLog)
	l, err := OpenRotatingLog(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.Repeat("x", 39) + "\n") // 40 bytes
	for i := 0; i < 10; i++ {
		if _, err := l.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.Write(bytes.Repeat([]byte("y"), 500)); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	for _, f := range []string{p, p + ".1"} {
		fi, err := os.Stat(f)
		if err != nil || fi.Size() > 100 || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, fi, err)
		}
	}
	if _, err := os.Stat(p + ".2"); !os.IsNotExist(err) {
		t.Fatal("a third log file was kept")
	}
}

func TestParsePmsetBatt(t *testing.T) {
	for out, want := range map[string]bool{
		"Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t100%; charged;\n":  true,
		"Now drawing from 'Battery Power'\n -InternalBattery-0\t80%; discharging;\n": false,
		"Now drawing from 'UPS Power'\n":                                             false,
	} {
		got, err := parsePmsetBatt(out)
		if err != nil || got != want {
			t.Errorf("%q: %v %v", out, got, err)
		}
	}
	if _, err := parsePmsetBatt("garbage"); err == nil {
		t.Error("parsed garbage")
	}
	if got := strings.Join(IdleSleepCommand(42), " "); got != "/usr/bin/caffeinate -i -w 42" {
		t.Errorf("idle sleep command %q", got)
	}
}

// The guest agent keeps one idle connection per port and redials after a session, so the newest proven
// connection replaces (and closes) the older one. The open byte follows the proof, never precedes it.
func TestPairerNewestGuestConnWinsAndOpenByte(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	p := newPairer(DesktopPort, key, []byte{DesktopOpenByte})
	l := &chanListener{ch: make(chan net.Conn)}
	go p.acceptGuest(l)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oldHost, oldPipe := net.Pipe()
	oldGuest, err := guestagent.PairedDial(func(context.Context) (net.Conn, error) { l.ch <- oldHost; return oldPipe, nil }, DesktopPort, key)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The newer connection answers by hand: the 40-byte frame, the proof, then the open byte.
	newHost, newGuest := net.Pipe()
	l.ch <- newHost
	frame := make([]byte, 40)
	_ = newGuest.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(newGuest, frame); err != nil || string(frame[:8]) != "WDYNPAIR" {
		t.Fatalf("challenge %q %v", frame, err)
	}
	if _, err := newGuest.Write(guestagent.PairProof(key, DesktopPort, frame[8:])); err != nil {
		t.Fatal(err)
	}
	// The stale idle connection was closed.
	_ = oldGuest.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := oldGuest.Read(make([]byte, 1)); err == nil {
		t.Fatal("old idle connection still open")
	}
	got := make(chan byte, 1)
	go func() {
		b := make([]byte, 1)
		_ = newGuest.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = newGuest.Read(b)
		got <- b[0]
	}()
	c, err := p.take(ctx)
	if err != nil || c != newHost {
		t.Fatalf("take = %v, %v", c, err)
	}
	if b := <-got; b != DesktopOpenByte {
		t.Fatalf("open byte %x", b)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, err := p.take(ctx2); !errors.Is(err, errNoGuest) {
		t.Fatalf("take with no guest: %v", err)
	}
	close(l.ch)
}

type chanListener struct{ ch chan net.Conn }

func (l *chanListener) Accept() (net.Conn, error) {
	c, ok := <-l.ch
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (l *chanListener) Close() error   { return nil }
func (l *chanListener) Addr() net.Addr { return &net.UnixAddr{} }

func TestParseContentRangeAndIPSWName(t *testing.T) {
	s, tot, err := parseContentRange("bytes 64-127/1000")
	if err != nil || s != 64 || tot != 1000 {
		t.Fatalf("%d %d %v", s, tot, err)
	}
	for _, bad := range []string{"", "bytes 1-2", "items 1-2/3", "bytes x-2/3"} {
		if _, _, err := parseContentRange(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if n, err := ipswName("https://updates.cdn-apple.com/x/UniversalMac_27.0_26A1_Restore.ipsw?a=b"); err != nil || n != "UniversalMac_27.0_26A1_Restore.ipsw" {
		t.Fatalf("%q %v", n, err)
	}
	for _, bad := range []string{"https://x/y.dmg", "https://x/.ipsw"} {
		if _, err := ipswName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A provisioning start error carrying the C side's mark wraps ErrVMLimit, keeping its text; any other error is
// returned as it was.
func TestClassifyProvisionStart(t *testing.T) {
	limit := errors.New("macvm provision: start: " + provisionVMLimitMark + "the limit")
	got := classifyProvisionStart(limit)
	if !errors.Is(got, ErrVMLimit) || !errors.Is(got, limit) {
		t.Fatalf("the VM limit start was classified as %v", got)
	}
	other := errors.New("macvm provision: start: boom")
	if out := classifyProvisionStart(other); errors.Is(out, ErrVMLimit) || out != other {
		t.Fatalf("another error was classified as %v", out)
	}
	elsewhere := errors.New("macvm provision: start: boom (" + provisionVMLimitMark + "not at the start)")
	if out := classifyProvisionStart(elsewhere); errors.Is(out, ErrVMLimit) || out != elsewhere {
		t.Fatalf("a mark elsewhere in the text was classified as %v", out)
	}
	if classifyProvisionStart(nil) != nil {
		t.Fatal("nil was classified")
	}
}

// The install process's stderr is kept to its last stderrTail bytes however
// much it writes, in any size of write.
func TestTailWriterKeepsOnlyTheTail(t *testing.T) {
	var all []byte
	w := &tailWriter{}
	for i, n := 0, 1; i < 400; i, n = i+1, n*7%5000+1 {
		p := bytes.Repeat([]byte{byte('a' + i%26)}, n)
		all = append(all, p...)
		if k, err := w.Write(p); k != n || err != nil {
			t.Fatalf("write %d: %d %v", n, k, err)
		}
		if len(w.b) > stderrTail || cap(w.b) > 2*stderrTail {
			t.Fatalf("after %d bytes the tail holds %d (cap %d)", len(all), len(w.b), cap(w.b))
		}
	}
	if !bytes.Equal(w.b, all[len(all)-stderrTail:]) {
		t.Fatal("the tail is not the last bytes written")
	}
}

// TestDefaultRootRefusesARelativeHome pins the guard: the runner deletes bases and the cache under this root.
func TestDefaultRootRefusesARelativeHome(t *testing.T) {
	t.Setenv("HOME", "relative/home")
	if r, err := DefaultRoot(); err == nil {
		t.Fatalf("DefaultRoot() = %q under a relative $HOME", r)
	}
	t.Setenv("HOME", "/Users/someone")
	if r, err := DefaultRoot(); err != nil || r != "/Users/someone/Library/Application Support/Wardyn/"+RootDirName {
		t.Fatalf("DefaultRoot() = %q, %v", r, err)
	}
}

// Shares are share n at ShareTag(n), absolute and clean, outside the base image; the plan carries
// them as given and CheckPlan holds an engine to the same shape.
func TestBuildPlanShares(t *testing.T) {
	s := testSpec(t)
	s.Shares = []Share{{Tag: ShareTag(0), HostPath: "/Users/me/proj", Target: "/home/agent/proj", ReadOnly: true},
		{Tag: ShareTag(1), HostPath: "/Users/me/out", Target: "/home/agent/out"}}
	p, err := BuildPlan(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Shares) != 2 || p.Shares[0] != s.Shares[0] || p.Shares[1] != s.Shares[1] {
		t.Fatalf("shares = %+v", p.Shares)
	}
	if err := CheckPlan(p); err != nil {
		t.Fatal(err)
	}
	for name, sh := range map[string][]Share{
		"duplicate tag":    {{Tag: ShareTag(0), HostPath: "/a"}, {Tag: ShareTag(0), HostPath: "/b"}},
		"other tag":        {{Tag: "macOSGuestAutomountTag", HostPath: "/a"}},
		"relative path":    {{Tag: ShareTag(0), HostPath: "a/b"}},
		"uncleaned path":   {{Tag: ShareTag(0), HostPath: "/a/../b"}},
		"host root":        {{Tag: ShareTag(0), HostPath: "/"}},
		"inside the base":  {{Tag: ShareTag(0), HostPath: filepath.Join(s.BaseDir, "x")}},
		"holding the base": {{Tag: ShareTag(0), HostPath: filepath.Dir(s.BaseDir)}},
	} {
		bad := testSpec(t)
		bad.BaseDir = s.BaseDir
		bad.Shares = sh
		if _, err := BuildPlan(bad); err == nil {
			t.Errorf("BuildPlan accepted %s", name)
		}
		q := p
		q.Shares = sh
		if name != "inside the base" && name != "holding the base" && CheckPlan(q) == nil {
			t.Errorf("CheckPlan accepted %s", name)
		}
	}
}

// The helper's backstop refuses a share whose recorded path now resolves elsewhere, or is gone.
func TestRecheckSharesRefusesASwappedPath(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, other := filepath.Join(root, "proj"), filepath.Join(root, "other")
	for _, d := range []string{dir, other} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	shares := []Share{{Tag: ShareTag(0), HostPath: dir}}
	if err := recheckShares(shares); err != nil {
		t.Fatalf("an unchanged share was refused: %v", err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, dir); err != nil {
		t.Fatal(err)
	}
	if err := recheckShares(shares); err == nil {
		t.Fatal("a share swapped for a link was accepted")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recheckShares(shares); err == nil {
		t.Fatal("a share that is now a file was accepted")
	}
}

// Only a run's VM shares folders: the installer's VM refuses any.
func TestInstallVMRefusesShares(t *testing.T) {
	s := testSpec(t)
	s.Shares = []Share{{Tag: ShareTag(0), HostPath: "/a"}}
	if err := installVM(context.Background(), nil, s, "", nil); !errors.Is(err, errSharesOutsideRun) {
		t.Fatalf("installVM = %v, want errSharesOutsideRun", err)
	}
}
