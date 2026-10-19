// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The sandbox chooses every remote name, and the report is read in a terminal.

func TestSyncEntryName_RefusesControlCharacters(t *testing.T) {
	for _, n := range []string{"a\x1bb", "a\rb", "a\nb", "a\x07", "\x1b]52;c;ZXZpbA==\x07", "a\tb", "a\x7f", "a\u0085b"} {
		err := syncEntryName(n)
		if err == nil || !strings.Contains(err.Error(), "control character") {
			t.Errorf("syncEntryName(%q) = %v, want a control-character refusal", n, err)
		}
	}
	for _, n := range []string{"plain.txt", "a b", "héllo wörld.txt", "日本語", "emoji-\U0001F600"} {
		if err := syncEntryName(n); err != nil {
			t.Errorf("syncEntryName(%q) refused: %v", n, err)
		}
	}
}

func TestPrintSyncReport_PlainNamesPrintAsTheyAre(t *testing.T) {
	rep := newSyncReport()
	rep.Pushed = []string{"a.txt"}
	rep.Conflicts = []string{"dir/héllo wörld.txt"}
	rep.refuse("日本語/x", "symlink")
	var b bytes.Buffer
	printSyncReport(&b, rep)
	want := "pushed 1 file(s) (0 bytes), pulled 0 file(s) (0 bytes)\n" +
		"conflict (changed on both sides; the laptop's copy wins, the sandbox's is not pulled): dir/héllo wörld.txt\n" +
		"refused (symlink): 日本語/x\n"
	if b.String() != want {
		t.Errorf("report = %q, want %q", b.String(), want)
	}
}

func TestPrintSyncReport_QuotesNamesWithNonPrintableCharacters(t *testing.T) {
	rep := newSyncReport()
	rep.Conflicts = []string{"c\x1b[2J\rfake"}
	rep.refuse("r\nrefused (x): fake", "symlink")
	rep.refuse("bidi\u202eexe", "symlink")
	var b bytes.Buffer
	printSyncReport(&b, rep)
	want := "pushed 0 file(s) (0 bytes), pulled 0 file(s) (0 bytes)\n" +
		"conflict (changed on both sides; the laptop's copy wins, the sandbox's is not pulled): \"c\\x1b[2J\\rfake\"\n" +
		"refused (symlink): \"r\\nrefused (x): fake\"\n" +
		"refused (symlink): \"bidi\\u202eexe\"\n"
	if b.String() != want {
		t.Errorf("report = %q, want %q", b.String(), want)
	}
}

func TestSyncGateway_ControlCharacterNamesAreRefusedNotSyncedOrPrintedRaw(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	hostile := []string{"osc\x1b]52;c;ZXZpbA==\x07", "erase\r\x1b[2Kfake", "forged\nrefused (x): y", "reg\x1bfile"}
	if err := os.Symlink("/etc/passwd", filepath.Join(box, hostile[0])); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(box, hostile[1]), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(box, hostile[2])); err != nil {
		t.Fatal(err)
	}
	putSyncFile(t, filepath.Join(box, hostile[3]), "x", 0o644)
	putSyncFile(t, filepath.Join(box, "ok.txt"), "fine", 0o644)

	rep := mustPass(t, sy)
	if !slices.Equal(rep.Pulled, []string{"ok.txt"}) {
		t.Errorf("pulled = %v, want only ok.txt", rep.Pulled)
	}
	if len(rep.Refused) != len(hostile) {
		t.Fatalf("refused = %+v, want %d refusals", rep.Refused, len(hostile))
	}
	for _, r := range rep.Refused {
		if strings.ContainsFunc(r.Path+r.Reason, func(c rune) bool { return c < ' ' || c == 0x7f }) || !strings.Contains(r.Reason, "control character") {
			t.Errorf("refusal %+v: want a control-character refusal with no raw control byte", r)
		}
	}
	var out bytes.Buffer
	printSyncReport(&out, rep)
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if !strings.HasPrefix(line, "pushed ") && !strings.HasPrefix(line, "refused (entry name ") {
			t.Errorf("unexpected report line %q", line)
		}
	}
	if strings.ContainsAny(strings.ReplaceAll(out.String(), "\n", ""), "\x1b\r\x07") {
		t.Errorf("report holds a raw control byte: %q", out.String())
	}
	if ents, _ := os.ReadDir(root); len(ents) != 1 {
		t.Errorf("local tree = %v, want only ok.txt", ents)
	}
}

// A sandbox that lists a huge directory must not be able to fill the laptop's
// memory: the listing is cut off by its byte budget, well before the entry cap.
func TestSyncGateway_AHugeDirectoryListingIsCutOffByItsByteBudget(t *testing.T) {
	g := newSyncRig(t)
	g.mkSandboxDir(syncRemote)
	g.rn.bigDir = syncRemote
	sy, _, _ := g.syncer(syncRemote, false)
	_, err := sy.pass(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the listing is larger than") {
		t.Fatalf("err = %v, want the listing refused for its size", err)
	}
	if n := g.rn.served.Load(); n >= maxSyncEntries {
		t.Errorf("the sandbox served %d entries, want the listing cut off before the %d entry cap", n, maxSyncEntries)
	}
}

// Text the sandbox supplies reaches the terminal too: refusal reasons, the
// sftp-server's stderr, its start directory.

const hostileText = "x\x1b]52;c;ZXZpbA==\x07\r\x1b[2K\nrefused (forged): y"

func rawControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r == 0x7f })
}

func TestPrintSyncReport_ReasonsPrintAsTheyAreOrQuoted(t *testing.T) {
	rep := newSyncReport()
	rep.refuse("a", "unreadable: open /x: permission denied")
	rep.refuse("b", "list é: "+"日本語 ok")
	rep.refuse("c", hostileText)
	var b bytes.Buffer
	printSyncReport(&b, rep)
	want := "pushed 0 file(s) (0 bytes), pulled 0 file(s) (0 bytes)\n" +
		"refused (unreadable: open /x: permission denied): a\n" +
		"refused (list é: 日本語 ok): b\n" +
		"refused (" + strconv.QuoteToGraphic(hostileText) + "): c\n"
	if b.String() != want {
		t.Errorf("report = %q, want %q", b.String(), want)
	}
}

func TestSyncExitError_QuotesTheSandboxsStderr(t *testing.T) {
	if err := syncExitError(3, " boom \n"); err == nil || err.Error() != "sync: the sandbox's sftp-server exited 3: boom" {
		t.Errorf("plain stderr = %v", err)
	}
	if err := syncExitError(3, hostileText); err == nil || rawControl(err.Error()) {
		t.Errorf("hostile stderr = %q, want no raw control byte", err)
	}
}

func TestSyncGateway_SandboxErrorTextNeverReachesTheTerminalRaw(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	g.mkSandboxDir(syncRemote + "/sub")
	putSyncFile(t, filepath.Join(box, "secret.txt"), "x", 0o644)
	putSyncFile(t, filepath.Join(box, "ok.txt"), "fine", 0o644)
	g.rn.failMsg = map[string]string{"sub": hostileText, "secret.txt": hostileText}
	g.rn.exitCode, g.rn.stderr = 3, hostileText
	sy, sess, _ := g.syncer(syncRemote, true)

	rep := mustPass(t, sy)
	var out bytes.Buffer
	printSyncReport(&out, rep)
	if len(rep.Refused) != 2 || !slices.Equal(rep.Pulled, []string{"ok.txt"}) {
		t.Errorf("report = %+v, want sub and secret.txt refused, ok.txt pulled", rep)
	}
	if rawControl(strings.ReplaceAll(out.String(), "\n", "")) || strings.Count(out.String(), "\n") != 3 {
		t.Errorf("report = %q, want three lines and no raw control byte", out.String())
	}
	if err := sess.close(); err == nil || rawControl(err.Error()) || !strings.Contains(err.Error(), "exited 3") {
		t.Errorf("close = %q, want the exit status and no raw control byte", err)
	}
}

// closedChannel is a channel whose Close is all refusal needs.
type closedChannel struct{ ssh.Channel }

func (closedChannel) Close() error { return nil }

func TestSyncSession_RefusalQuotesTheGatewaysStderr(t *testing.T) {
	done := make(chan struct{})
	close(done)
	s := &syncSession{ch: closedChannel{}, exited: done, errDone: done}
	_, _ = s.stderr.Write([]byte(hostileText))
	if err := s.refusal(); rawControl(err.Error()) {
		t.Errorf("refusal = %q, want no raw control byte", err)
	}
}

func TestSyncGateway_HostileStartDirectoryIsQuoted(t *testing.T) {
	g := newSyncRig(t)
	g.mkSandboxDir("/home/agent/work")
	g.mkSandboxDir("/home/agent/x\x1b[2Kevil")
	g.rn.start = "/home/agent/x\x1b[2Kevil"
	_, err := openSyncSession(g.dial(), "/home/agent/work", "push")
	if err == nil || !strings.Contains(err.Error(), "started in") || rawControl(err.Error()) {
		t.Fatalf("err = %q, want the start-directory abort with no raw control byte", err)
	}
}
