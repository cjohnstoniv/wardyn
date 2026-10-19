// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k6_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A followed proxy log keeps the lapse line after the pod is gone (#3149): the stand-in kubectl prints
// the line on its first call and answers NotFound on every later one, as a deleted pod does.
func TestFollowProxyLogOutlivesThePod(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ -e " + filepath.Join(dir, "called") + " ]; then echo 'Error from server (NotFound): pods not found' >&2; exit 1; fi\n" +
		": > " + filepath.Join(dir, "called") + "\necho '" + lapseLine + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	l := (&orgEnv{adminKC: "kc", runsNS: "ns"}).followProxyLog(ctx, "r1")
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(l.String(), lapseLine); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("lapse line never captured: %q", l.String())
		}
	}
	time.Sleep(3 * time.Second) // long enough for a second follow, which would log NotFound
	if got := l.String(); !strings.Contains(got, lapseLine) || strings.Contains(got, "NotFound") {
		t.Fatalf("want the lapse line kept and no re-follow once it is captured, got %q", got)
	}
}
