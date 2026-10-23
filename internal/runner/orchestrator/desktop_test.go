// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type desktopSubstrate struct {
	*fakeSubstrate
	dialled []string
}

type nopConn struct{}

func (nopConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopConn) Write(b []byte) (int, error) { return len(b), nil }
func (nopConn) Close() error                { return nil }

func (d *desktopSubstrate) DialDesktop(_ context.Context, ref string) (io.ReadWriteCloser, error) {
	d.rec(&d.dialled, ref)
	return nopConn{}, nil
}

// A desktop dial reaches the substrate that owns the ref; one with no displays answers
// ErrDesktopUnsupported.
func TestOrchestrator_DialDesktop(t *testing.T) {
	ctx := context.Background()
	other := &fakeSubstrate{name: "smolvm", classes: []types.ConfinementClass{types.CC3}, refPrefix: "vm-"}
	docker := &desktopSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}, refPrefix: "wardyn-agent-"}}
	o := New(other, docker)
	sb, err := o.CreateSandbox(ctx, specFor(types.CC1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.DialDesktop(ctx, sb.Ref); err != nil || len(docker.dialled) != 1 || docker.dialled[0] != sb.Ref {
		t.Fatalf("DialDesktop = %v, dialled %v", err, docker.dialled)
	}
	plain := New(&fakeSubstrate{name: "k8s", classes: []types.ConfinementClass{types.CC1}})
	if _, err := plain.DialDesktop(ctx, "wardyn-agent-y"); !errors.Is(err, runner.ErrDesktopUnsupported) {
		t.Fatalf("a substrate with no desktops = %v, want ErrDesktopUnsupported", err)
	}
}
