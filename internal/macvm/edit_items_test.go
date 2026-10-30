// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// readBackGuest is the edit's guest whose verify-cert fails with verifyErr when it is set: a read-back that
// cannot finish. A stat answers that its path is gone unless it is in left; an rm of rmFails exits 1 in the
// recheck's boot. seq
// is the pushes, the file runs (rm, mkdir, ditto, chown, stat), posture reads and verify-certs in order,
// each with the codes set by then.
type readBackGuest struct {
	*editGuest
	verifyErr error
	left      []string
	rmFails   string
	seq       []string
}

func (g *readBackGuest) note(what string) {
	g.editGuest.mu.Lock()
	n := len(g.codes)
	g.editGuest.mu.Unlock()
	g.seq = append(g.seq, fmt.Sprintf("%s after %d codes", what, n))
}

// inRecheck says whether the recheck's boot, which sets the second code, has begun.
func (g *readBackGuest) inRecheck() bool {
	g.editGuest.mu.Lock()
	defer g.editGuest.mu.Unlock()
	return len(g.codes) >= 2
}

func (g *readBackGuest) Posture(ctx context.Context) (guestagent.ToolsPosture, error) {
	g.note("posture")
	return g.editGuest.Posture(ctx)
}

func (g *readBackGuest) Push(_ context.Context, dest string, mode fs.FileMode, r io.Reader, _ int64) error {
	_, _ = io.Copy(io.Discard, r)
	g.note(fmt.Sprintf("push %s %o", dest, mode))
	return nil
}

func (g *readBackGuest) Run(ctx context.Context, argv []string) (string, int, error) {
	switch argv[0] {
	case "/bin/rm", "/bin/mkdir", "/usr/bin/ditto", "/usr/sbin/chown", "/usr/bin/stat":
		g.note("run " + strings.Join(argv, " "))
	}
	if argv[0] == "/usr/bin/stat" && !slices.Contains(g.left, argv[1]) {
		return "", 1, nil
	}
	if argv[0] == "/bin/rm" && argv[2] == g.rmFails && g.inRecheck() {
		return "", 1, nil
	}
	if len(argv) > 1 && argv[1] == "verify-cert" {
		g.note("verify-cert")
		if g.verifyErr != nil {
			return "", 0, g.verifyErr
		}
	}
	return g.editGuest.Run(ctx, argv)
}

// newItemsEdit is an edit of a base whose tools.json holds a staged profile and a CA read as not trusted,
// and no guest posture; the install list names the profile's file on this Mac, whose path it returns.
func newItemsEdit(t *testing.T) (*editFix, *readBackGuest, string) {
	t.Helper()
	e := newEdit(t)
	rec, err := macvm.ReadToolsRecord(e.base)
	if err != nil {
		t.Fatal(err)
	}
	wifi, pin := itemFile(t, t.TempDir(), "wifi.mobileconfig", "<plist>wifi</plist>")
	e.opts.Items = macvm.BaseList{Version: 1, Items: []macvm.BaseItem{
		{Kind: macvm.ItemKindProfile, Name: "wifi", Path: wifi, SHA256: pin, Identifier: "com.example.wifi"}}}
	rec.Guest, rec.Items = nil, []macvm.ItemRecord{
		{Kind: macvm.ItemKindProfile, Name: "wifi", SHA256: pin, State: macvm.ItemStaged, Identifier: "com.example.wifi"},
		{Kind: macvm.ItemKindCA, Name: "orgca", SHA256: strings.Repeat("b", 64), State: macvm.ItemNotTrusted, SHA1: strings.Repeat("C", 40)},
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(e.base, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	g := &readBackGuest{editGuest: e.g}
	e.opts.Guest = func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
		e.boots++
		return g, func(context.Context) error { return nil }, nil
	}
	return e, g, wifi
}

// again readies e for another edit: its window's guest shuts down once the window is up.
func (e *editFix) again(t *testing.T) {
	boots := e.f.fb.Boots()
	opened0, _ := e.f.fb.Windows()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	wait := func(more func() bool) bool {
		for more() {
			select {
			case <-stop:
				return false
			case <-time.After(time.Millisecond):
			}
		}
		return true
	}
	go func() {
		noWindow := func() bool { opened, _ := e.f.fb.Windows(); return opened == opened0 }
		if wait(func() bool { return e.f.fb.Boots() == boots }) && wait(noWindow) {
			e.f.fb.Shutdown(nil)
		}
	}()
	e.out.Reset()
}

const (
	wifiPush  = "push /Library/Wardyn/stage/wifi/item.mobileconfig 600"
	wifiCopy  = "/Users/wardyn-admin/Library/Wardyn/wifi.mobileconfig"
	wifiLine  = "While signed in as wardyn-admin, open " + wifiCopy + ", then review and install the staged profile in System Settings."
	adminDir  = "/Users/wardyn-admin/Library/Wardyn"
	stageWifi = "/Library/Wardyn/stage/wifi"
)

// The edit's first boot stages each waiting profile again, 0600 in its 0700 stage directory as prepare
// does, then copies it into wardyn-admin's own 0700 directory (ditto keeps the 0600) and hands the copy and
// then the directory to wardyn-admin, and the edit prints the copy's path only. Nothing reaches the run
// user's home. The recheck's boot first removes the copies' directory, then the stage directory, each
// checked gone by a stat that must fail, and reads the items back (ReadBackItems) after the accounts and
// Remote Login, into the copy, whose tools.json is swapped in with it.
func TestEditBaseNamesProfilesThenReadsItemsBack(t *testing.T) {
	e, g, _ := newItemsEdit(t)
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	lines := e.lines()
	if len(lines) != 5 || lines[2] != wifiLine || lines[3] != macvm.EditCheckingLine {
		t.Fatalf("lines:\n%s", e.out.String())
	}
	var want []string
	for _, w := range []string{
		"run /bin/mkdir -p -m 0700 " + stageWifi, wifiPush,
		"run /bin/rm -rf " + adminDir, "run /bin/mkdir -m 0700 " + adminDir,
		"run /usr/bin/ditto " + stageWifi + "/item.mobileconfig " + wifiCopy,
		"run /usr/sbin/chown -h wardyn-admin " + wifiCopy, "run /usr/sbin/chown -h wardyn-admin " + adminDir,
	} {
		want = append(want, w+" after 1 codes")
	}
	for _, w := range []string{
		"run /bin/rm -rf " + adminDir, "run /usr/bin/stat " + adminDir, "run /bin/rm -rf " + stageWifi, "run /usr/bin/stat " + stageWifi,
		"posture", "verify-cert",
	} {
		want = append(want, w+" after 2 codes")
	}
	if !slices.Equal(g.seq, want) {
		t.Fatalf("the edit's boots ran\n%s\nwant\n%s", strings.Join(g.seq, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(strings.Join(g.seq, " "), "/Users/agent") || strings.Contains(e.out.String(), stageWifi) {
		t.Fatalf("the run user's home was touched, or the stage path printed:\n%s", e.out.String())
	}
	rec, err := macvm.ReadToolsRecord(e.base)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Guest == nil || len(rec.Items) != 2 || rec.Items[1].State != macvm.ItemTrusted || rec.Items[0].State != macvm.ItemStaged {
		t.Fatalf("the swapped-in base's record was not read back: %+v", rec)
	}
}

// A read-back that cannot finish discards the copy: runs keep the old base and its record. None of the
// discard lines (MAC-E09, E10, E11, E18) names it, so it is a plain error and no recheck is recorded.
func TestEditBaseDiscardsOnAFailedReadBack(t *testing.T) {
	e, g, _ := newItemsEdit(t)
	g.verifyErr = errors.New("guest agent gone")
	_, err := macvm.EditBase(context.Background(), e.opts)
	var discarded *macvm.EditDiscardedError
	if err == nil || !strings.Contains(err.Error(), "items read-back") || errors.As(err, &discarded) {
		t.Fatalf("err %v", err)
	}
	e.oldBaseKept(t)
	rec, err := macvm.ReadToolsRecord(e.base)
	if err != nil || rec.Guest != nil || rec.Items[1].State != macvm.ItemNotTrusted {
		t.Fatalf("the old base's record changed: %+v %v", rec, err)
	}
	m, _ := macvm.ReadyBase(e.base)
	if r, ok := macvm.ReadEditRecord(e.base, m); ok && r.Recheck == macvm.EditRecheckPassed {
		t.Errorf("a discarded edit was recorded as passed: %+v", r)
	}
}

// A second edit, after the first one's recheck removed the staged file, pushes it again from the install
// list and prints its path. With the file gone from this Mac, an edit pushes and prints nothing, and the
// profile stays staged.
func TestEditBaseRestagesTheWaitingProfiles(t *testing.T) {
	e, g, wifi := newItemsEdit(t)
	pushed := func() int {
		return len(slices.DeleteFunc(slices.Clone(g.seq), func(s string) bool { return !strings.HasPrefix(s, wifiPush+" ") }))
	}
	for i := range 2 {
		if i > 0 {
			e.again(t)
		}
		g.seq = nil
		if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
			t.Fatalf("edit %d: %v", i+1, err)
		}
		if pushed() != 1 || !slices.Contains(e.lines(), wifiLine) || !slices.Contains(g.seq, "run /bin/rm -rf "+stageWifi+" after "+fmt.Sprint(2*i+2)+" codes") {
			t.Fatalf("edit %d: ran %q, printed\n%s", i+1, g.seq, e.out.String())
		}
	}
	if err := os.Remove(wifi); err != nil {
		t.Fatal(err)
	}
	e.again(t)
	g.seq = nil
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if pushed() != 0 || slices.Contains(e.lines(), wifiLine) {
		t.Fatalf("with no file on this Mac: ran %q, printed\n%s", g.seq, e.out.String())
	}
	if rec, err := macvm.ReadToolsRecord(e.base); err != nil || rec.Items[0].State != macvm.ItemStaged {
		t.Fatalf("record %+v %v", rec, err)
	}
}

// A removal that fails, or that a stat finds undone, discards the edit like a failed read-back: a base that
// still holds wardyn-admin's copy or the staged file never reaches runs.
func TestEditBaseDiscardsOnAFailedRemoval(t *testing.T) {
	for name, set := range map[string]func(g *readBackGuest){
		"the copies' rm fails":           func(g *readBackGuest) { g.rmFails = adminDir },
		"the copies' dir is still there": func(g *readBackGuest) { g.left = []string{adminDir} },
		"the stage rm fails":             func(g *readBackGuest) { g.rmFails = stageWifi },
		"the stage dir is still there":   func(g *readBackGuest) { g.left = []string{stageWifi} },
	} {
		t.Run(name, func(t *testing.T) {
			e, g, _ := newItemsEdit(t)
			set(g)
			_, err := macvm.EditBase(context.Background(), e.opts)
			var discarded *macvm.EditDiscardedError
			if err == nil || !strings.Contains(err.Error(), "staged profiles") || errors.As(err, &discarded) {
				t.Fatalf("err %v", err)
			}
			e.oldBaseKept(t)
			m, _ := macvm.ReadyBase(e.base)
			if r, ok := macvm.ReadEditRecord(e.base, m); ok && r.Recheck == macvm.EditRecheckPassed {
				t.Errorf("a discarded edit was recorded as passed: %+v", r)
			}
		})
	}
}

// A profile the install list pins differently than the base was built with is not offered: no push, no
// line, one log line naming its file by path.
func TestEditBaseLogsAProfilePinnedDifferently(t *testing.T) {
	e, g, wifi := newItemsEdit(t)
	var log bytes.Buffer
	e.opts.Log = slog.New(slog.NewTextHandler(&log, nil))
	e.opts.Items.Items[0].SHA256 = strings.Repeat("d", 64)
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(g.seq, func(s string) bool { return strings.HasPrefix(s, wifiPush) }) || slices.Contains(e.lines(), wifiLine) {
		t.Fatalf("ran %q, printed\n%s", g.seq, e.out.String())
	}
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "path="+wifi) || strings.Contains(lines[0], "wifi</plist>") {
		t.Fatalf("log:\n%s", log.String())
	}
}
