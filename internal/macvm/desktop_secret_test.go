// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// openDesktop pairs one desktop.sock connection with a fake Screen Sharing that accepts password and
// returns the viewer's side, its ServerInit read when accepted is true, and Screen Sharing's side.
func openDesktop(t *testing.T, r *runRig, m *macvmtest.Machine, password string, accepted bool) (net.Conn, net.Conn) {
	t.Helper()
	guest, err := m.DialAgent(macvm.DesktopPort)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = guest.Close() })
	_ = guest.SetDeadline(time.Now().Add(10 * time.Second))
	served := make(chan struct{})
	go func() {
		defer close(served)
		open := make([]byte, 1)
		if _, err := io.ReadFull(guest, open); err != nil || open[0] != macvm.DesktopOpenByte {
			return
		}
		appleFake{version: "RFB 003.889\n", offered: []byte{2}, password: password}.serve(t, guest)
	}()
	viewer, err := net.Dial("unix", filepath.Join(r.sock, macvm.DesktopSock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = viewer.Close() })
	_ = viewer.SetDeadline(time.Now().Add(10 * time.Second))
	if accepted {
		(&shimRig{viewer: viewer, apple: guest}).viewerHandshake(t)
	} else {
		ended(t, viewer, "the viewer of a refused login")
	}
	<-served
	return viewer, guest
}

// waitLog waits for the helper log at path to hold want.
func waitLog(t *testing.T, path, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, _ := os.ReadFile(path); bytes.Contains(b, []byte(want)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helper log never said %q", want)
		}
	}
}

// The run's VNC password lives in the helper's memory only. A run that serves it on ctl.sock, logs in
// to Screen Sharing with it, has a login refused and logs the secret value by mistake leaves it in no
// file under the run dir, the helper log included; and the run's end wipes it.
func TestADesktopSessionLeavesThePasswordNowhereOnDisk(t *testing.T) {
	secret, err := macvm.NewDesktopSecret()
	if err != nil {
		t.Fatal(err)
	}
	var logf *macvm.RotatingLog
	r := startRun(t, &macvmtest.Engine{}, func(_ *macvm.RunConfig, o *macvm.RunOptions) {
		if err := os.MkdirAll(o.RunDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if logf, err = macvm.OpenRotatingLog(filepath.Join(o.RunDir, macvm.HelperLog), macvm.HelperLogMax); err != nil {
			t.Fatal(err)
		}
		o.Log = slog.New(slog.NewJSONHandler(logf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		o.Log.Debug("macvm: test logs the secret value itself", "password", secret)
		o.Desktop = &macvm.RFBShim{Credential: secret.Credential, Log: o.Log}
		o.DesktopPassword = secret
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pw, err := r.cc.DesktopPassword(ctx)
	if err != nil || len(pw) != 8 {
		t.Fatalf("desktop_password = %d characters, %v; want 8", len(pw), err)
	}
	r.attach(t)
	waitState(t, r.cc, macvm.StateRunning)
	m := r.eng.Machines()[0]

	logPath := filepath.Join(r.dir, macvm.HelperLog)
	viewer, apple := openDesktop(t, r, m, pw, true)
	cut := []byte{6, 0, 0, 0, 0, 0, 0, 2, 'h', 'i'}
	_, _ = viewer.Write(cut)
	if got := readFull(t, apple, len(cut)); !bytes.Equal(got, cut) {
		t.Fatalf("Screen Sharing got %v", got)
	}
	_ = viewer.Close()
	waitLog(t, logPath, "desktop session ended")
	openDesktop(t, r, m, "Zzzz9999", false) // Screen Sharing refuses the password
	waitLog(t, logPath, "Screen Sharing login")

	if err := r.cc.Stop(ctx, true, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := <-r.done; err != nil {
		t.Fatal(err)
	}
	_ = logf.Close()
	if _, err := secret.Credential(ctx); !errors.Is(err, macvm.ErrNoDesktopPassword) {
		t.Fatalf("after the run the password is still held: %v", err)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"desktop session ended", "Screen Sharing login", "[redacted]"} {
		if !bytes.Contains(log, []byte(want)) {
			t.Fatalf("the helper log has no %q line, so it proves nothing:\n%s", want, log)
		}
	}
	files := 0
	err = filepath.WalkDir(r.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		if bytes.Contains(b, []byte(pw)) {
			t.Errorf("%s holds the password", path)
		}
		return nil
	})
	if err != nil || files < 2 {
		t.Fatalf("walked %d files: %v", files, err)
	}
}
