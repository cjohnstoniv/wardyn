// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// passwordHandler answers only desktop_password; the other ops are not reached here.
type passwordHandler struct{ controlHandler }

func (passwordHandler) desktopPassword() (string, error) { return "Abcd2345", nil }

// desktop_password is answered to a peer of the helper's own uid (SO_PEERCRED here, LOCAL_PEERCRED
// on a Mac) and refused, with no password in the reply, to any other.
func TestDesktopPasswordChecksThePeersUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), ControlSock)
	l, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go serveControl(l, passwordHandler{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cc, err := DialControl(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if pw, err := cc.DesktopPassword(ctx); err != nil || pw != "Abcd2345" {
		t.Fatalf("own uid: password %q, %v", pw, err)
	}
	_ = cc.Close()

	t.Cleanup(func() { controlOwnerUID.Store(int64(os.Getuid())) })
	controlOwnerUID.Store(int64(os.Getuid() + 1))
	cc, err = DialControl(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	pw, err := cc.DesktopPassword(ctx)
	if err == nil || pw != "" || !strings.Contains(err.Error(), "not this helper's user") {
		t.Fatalf("a stranger's read: password %q, err %v; want refused", pw, err)
	}
	// The refusal closes the connection.
	if _, err := cc.Status(ctx); err == nil {
		t.Fatal("the connection outlived the refusal")
	}
}
