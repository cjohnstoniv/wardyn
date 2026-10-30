// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

const noPairKey = "macvm: this macOS base has no pairing key; prepare it again"

// A base written before pairing keys is format 1, and this helper refuses it: it is prepared again.
func TestReadyBaseRefusesFormat1(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, macvm.ManifestFile), []byte(`{"format":1,"state":"ready"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := macvm.ReadyBase(base)
	if want := "macvm: this Mac's macOS base needs an update (format 1, helper accepts 2)"; err == nil || err.Error() != want {
		t.Fatalf("ReadyBase(format 1) = %v, want %q", err, want)
	}
}

// The key is a regular file of exactly 32 bytes, mode 0600; anything else is no key.
func TestReadPairKeyRefusesAnythingButARegular0600KeyFile(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	for name, c := range map[string]func(dir string) error{
		"absent":     func(string) error { return nil },
		"group-read": func(dir string) error { return os.WriteFile(filepath.Join(dir, macvm.PairKeyFile), key, 0o640) },
		"world-read": func(dir string) error { return os.WriteFile(filepath.Join(dir, macvm.PairKeyFile), key, 0o604) },
		"short":      func(dir string) error { return os.WriteFile(filepath.Join(dir, macvm.PairKeyFile), key[:31], 0o600) },
		"long": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, macvm.PairKeyFile), append(key, 0), 0o600)
		},
		"a directory":  func(dir string) error { return os.Mkdir(filepath.Join(dir, macvm.PairKeyFile), 0o700) },
		"a link to it": func(dir string) error { return writeLinkedKey(dir, key) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := c(dir); err != nil {
				t.Fatal(err)
			}
			if got, err := macvm.ReadPairKey(dir); err == nil || err.Error() != noPairKey {
				t.Fatalf("ReadPairKey = %x, %v; want %q", got, err, noPairKey)
			}
		})
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, macvm.PairKeyFile), key, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := macvm.ReadPairKey(dir); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("ReadPairKey of a good key = %x, %v", got, err)
	}
}

func writeLinkedKey(dir string, key []byte) error {
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, key, 0o600); err != nil {
		return err
	}
	return os.Symlink(target, filepath.Join(dir, macvm.PairKeyFile))
}

// hostKey is the base's host copy of its key, which must be a good one.
func hostKey(t *testing.T, dir string) []byte {
	t.Helper()
	key, err := macvm.ReadPairKey(dir)
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	return key
}

// The key prepare made survives a tools update, a derivation and an edit: each one's copy carries the
// same key as the guest disk it cloned, and the swap's file check takes it as a base file.
func TestThePairKeySurvivesTheBaseLifecycle(t *testing.T) {
	t.Run("tools update and derivation", func(t *testing.T) {
		f, tf, _ := stage2Prep(t)
		if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
			t.Fatal(err)
		}
		base := f.opts.Layout.BaseDir(0)
		key := hostKey(t, base)
		tf.claude = []byte("claude-binary-2.1.311")
		tf.tooling.Harnesses[0].Version, tf.tooling.Harnesses[0].SHA256, tf.tooling.Harnesses[0].Size = "2.1.311", shaHex(tf.claude), int64(len(tf.claude))
		tf.opts.Tooling = tf.tooling
		g := newGuest()
		g.version["/usr/local/bin/claude"] = "claude 2.1.311"
		if _, changed, err := macvm.UpdateTools(context.Background(), updateOpts(f, tf, g, "0.10.0")); err != nil || !changed {
			t.Fatalf("update: changed %v err %v", changed, err)
		}
		if got := hostKey(t, base); !bytes.Equal(got, key) {
			t.Fatal("a tools update changed the base's key, but not its guest disk's")
		}
		p, pin := itemFile(t, t.TempDir(), "xcode.cfg", "xcode-config")
		list := macvm.BaseList{Version: 1, Items: []macvm.BaseItem{{Kind: "file", Name: "xcode-cfg", Path: p, SHA256: pin, Dest: "/Library/Application Support/Org/xcode.cfg"}}}
		ig := newItemGuest()
		tf.opts.Items = list
		o := updateOpts(f, tf, ig.guest, "0.10.0")
		o.Stage, o.ItemsDigest = tf.itemStage(ig), list.Digest()
		if _, _, err := macvm.DeriveBase(context.Background(), o, "xcode-16"); err != nil {
			t.Fatal(err)
		}
		if got := hostKey(t, f.opts.Layout.VariantDir("xcode-16")); !bytes.Equal(got, key) {
			t.Fatal("a derived base holds another key than the Standard it cloned")
		}
	})
	t.Run("edit", func(t *testing.T) {
		e := newEdit(t)
		key := hostKey(t, e.base)
		if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
			t.Fatal(err)
		}
		if got := hostKey(t, e.base); !bytes.Equal(got, key) {
			t.Fatal("an edited base holds another key than the base it copied")
		}
	})
}

// bootRun starts a run of base and returns the error it fails with, or nil when it booted.
func bootRun(t *testing.T, base string) error {
	t.Helper()
	r := startRun(t, &macvmtest.Engine{}, func(c *macvm.RunConfig, _ *macvm.RunOptions) { c.BaseDir = base })
	select {
	case err := <-r.done:
		return err
	case <-time.After(3 * time.Second):
		return nil
	}
}

func removeKey(t *testing.T, dir string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, macvm.PairKeyFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

// A base of this format whose key is gone boots nothing, whichever way it was made: the pairer never
// runs without a key.
func TestBootRefusesAMissingPairKey(t *testing.T) {
	refused := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), noPairKey) {
			t.Fatalf("boot without a key: %v, want %q", err, noPairKey)
		}
	}
	t.Run("plain", func(t *testing.T) {
		f, _, _ := stage2Prep(t)
		if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
			t.Fatal(err)
		}
		base := f.opts.Layout.BaseDir(0)
		removeKey(t, base)
		refused(t, bootRun(t, base))
	})
	t.Run("derived", func(t *testing.T) {
		f, tf, _ := stage2Prep(t)
		if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
			t.Fatal(err)
		}
		p, pin := itemFile(t, t.TempDir(), "xcode.cfg", "xcode-config")
		list := macvm.BaseList{Version: 1, Items: []macvm.BaseItem{{Kind: "file", Name: "xcode-cfg", Path: p, SHA256: pin, Dest: "/Library/Application Support/Org/xcode.cfg"}}}
		ig := newItemGuest()
		tf.opts.Items = list
		o := updateOpts(f, tf, ig.guest, "0.10.0")
		o.Stage, o.ItemsDigest = tf.itemStage(ig), list.Digest()
		if _, _, err := macvm.DeriveBase(context.Background(), o, "xcode-16"); err != nil {
			t.Fatal(err)
		}
		dir := f.opts.Layout.VariantDir("xcode-16")
		removeKey(t, dir)
		refused(t, bootRun(t, dir))
	})
	t.Run("edited", func(t *testing.T) {
		e := newEdit(t)
		if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
			t.Fatal(err)
		}
		removeKey(t, e.base)
		refused(t, bootRun(t, e.base))
	})
	t.Run("stage-2 boot", func(t *testing.T) {
		eng := &macvmtest.Engine{}
		cfg := macvm.Stage2BootConfig{Engine: eng, SockRoot: macvmtest.SockRoot(t), SockKey: macvm.JobSockKey("0123456789abcdef01234567"),
			SockFstat: macvmtest.SockFstat, GuestWait: time.Second, NetWait: 2 * time.Second}
		dir := t.TempDir()
		for _, f := range []string{macvm.DiskFile, macvm.AuxFile} {
			_ = os.WriteFile(filepath.Join(dir, f), nil, 0o600)
		}
		spec := macvm.MachineSpec{CPUs: 4, MemoryBytes: 8 << 30, MinCPUs: 2, MinMemoryBytes: 4 << 30, HardwareModel: []byte("hw"),
			MachineIdentifier: []byte("id"), DiskPath: filepath.Join(dir, macvm.DiskFile), AuxPath: filepath.Join(dir, macvm.AuxFile)}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, err := cfg.ToolsBoot()(ctx, spec)
		refused(t, err)
		if len(eng.Machines()) != 0 {
			t.Fatal("a VM was built for a boot with no pairing key")
		}
	})
}

// An interrupted prepare's partial base is discarded with its key.
func TestDiscardedInstallTakesItsPairKey(t *testing.T) {
	f, _, _ := stage2Prep(t)
	f.opts.Stage2 = &failingStage2{next: f.opts.Stage2}
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil {
		t.Fatal("the cut-short prepare succeeded")
	}
	base := f.opts.Layout.BaseDir(0)
	hostKey(t, base) // stage 1 ran, so the partial base holds its key
	f.eng.InstallFunc = func(context.Context, *macvmtest.Machine, string, func(float64)) error {
		return errors.New("restore failed")
	}
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil {
		t.Fatal("prepare with a failing restore succeeded")
	}
	if _, err := os.Lstat(filepath.Join(base, macvm.PairKeyFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the discarded install's key is still there: %v", err)
	}
}

// keyForms are the ways a key could show in text: raw, hex and base64.
func keyForms(key []byte) []string {
	return []string{string(key), hex.EncodeToString(key), strings.ToUpper(hex.EncodeToString(key)),
		base64.StdEncoding.EncodeToString(key), base64.RawStdEncoding.EncodeToString(key), base64.RawURLEncoding.EncodeToString(key)}
}

func leaks(text string, key []byte) bool {
	for _, f := range keyForms(key) {
		if strings.Contains(text, f) {
			return true
		}
	}
	return false
}

// Prepare gives stage 1 the key, which writes it to the guest, and keeps the same bytes as the base's host
// copy. Nothing else prepare or a run of the base writes holds the key: no log line, event, argv,
// tools.json, manifest, status or control event.
func TestPrepareKeysTheGuestAndARunLeaksTheKeyNowhere(t *testing.T) {
	f, srv := stage1Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	key := hostKey(t, base)
	if got := srv.File(guestagent.MacPairKeyPath); !bytes.Equal(got, key) {
		t.Fatalf("the guest's %s (%d bytes) is not the base's host copy", guestagent.MacPairKeyPath, len(got))
	}
	var text strings.Builder
	text.WriteString(prepareText(f, nil))
	for _, e := range srv.Execs() {
		text.WriteString(e.Line + "\n")
	}
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && d.Name() != macvm.PairKeyFile {
			b, _ := os.ReadFile(p)
			text.Write(b)
		}
		return nil
	})

	var logMu sync.Mutex
	var logs bytes.Buffer
	eng := &macvmtest.Engine{}
	r := startRun(t, eng, func(c *macvm.RunConfig, o *macvm.RunOptions) {
		c.BaseDir = base
		o.Log = slog.New(slog.NewJSONHandler(lockedWriter{&logMu, &logs}, nil))
	})
	evc, err := macvm.DialControl(context.Background(), filepath.Join(r.sock, macvm.ControlSock))
	if err != nil {
		t.Fatal(err)
	}
	defer evc.Close()
	evs, err := evc.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.attach(t)
	waitState(t, r.cc, macvm.StateRunning)
	m := eng.Machines()[0]
	real := dialAgent(t, m, macvm.AgentPort, key)
	closedWithin(t, hostClosed(dialAgent(t, m, macvm.AgentPort, wrongKey)), 10*time.Second, "a dialer without the key")
	host, err := net.Dial("unix", filepath.Join(r.sock, macvm.AgentSock))
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	go func() { _, _ = host.Write([]byte("ping")) }()
	_ = real.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, 4)
	if _, err := real.Read(b); err != nil || string(b) != "ping" {
		t.Fatalf("agent.sock did not reach the real agent: %q %v", b, err)
	}
	st, err := r.cc.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sj, _ := json.Marshal(st)
	text.Write(sj)
	if err := r.cc.Stop(context.Background(), true, time.Second); err != nil {
		t.Fatal(err)
	}
	<-r.done
	for ev := range evs {
		ej, _ := json.Marshal(ev)
		text.Write(ej)
	}
	logMu.Lock()
	text.Write(logs.Bytes())
	refusal := strings.Contains(logs.String(), "macvm: refused a guest connection without the pairing proof")
	logMu.Unlock()
	if !refusal {
		t.Error("the helper's log does not show the refused pairing proof")
	}
	if leaks(text.String(), key) {
		t.Fatalf("the pairing key is in a log, event, argv, record or status:\n%s", text.String())
	}
}
