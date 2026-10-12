// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func TestSealRequiresDrainedWriterAndExactReceipt(t *testing.T) {
	s, dir, id := testSpool(t)
	ctx := t.Context()
	if err := s.Append(ctx, decision(id)); err != nil {
		t.Fatal(err)
	}
	accepted, err := s.Receipt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sealed, err := s.Seal(ctx, accepted); err != nil || sealed {
		t.Fatal("unacknowledged evidence sealed")
	}
	if err := s.Replay(ctx, func(context.Context, []Record) error { return nil }); err != nil {
		t.Fatal(err)
	}
	accepted, err = s.Receipt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(filepath.Join(dir, "writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := unix.Flock(int(writer.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if sealed, err := s.Seal(ctx, accepted); err != nil || sealed {
		t.Fatal("live producer sealed")
	}
	if err := unix.Flock(int(writer.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	// A queued tail wins the manifest lock before seal: retirement must defer.
	if err := s.AppendAdmitted(ctx, decision(id)); err != nil {
		t.Fatal(err)
	}
	if sealed, err := s.Seal(ctx, accepted); err != nil || sealed {
		t.Fatal("late tail discarded by old receipt")
	}
	if err := s.Replay(ctx, func(context.Context, []Record) error { return nil }); err != nil {
		t.Fatal(err)
	}
	accepted, err = s.Receipt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*types.RunnerSpoolReceipt){
		func(p *types.RunnerSpoolReceipt) { p.SpoolID = uuid.New() },
		func(p *types.RunnerSpoolReceipt) { p.Seq++ },
		func(p *types.RunnerSpoolReceipt) { p.Hash = "foreign" },
		func(p *types.RunnerSpoolReceipt) { p.ReportedDrops++ },
	} {
		bad := accepted
		change(&bad)
		if sealed, err := s.Seal(ctx, bad); err != nil || sealed {
			t.Fatal("different settled receipt sealed")
		}
	}
	if sealed, err := s.Seal(ctx, accepted); err != nil || !sealed {
		t.Fatalf("settled seal: %v %v", sealed, err)
	}
	reopened, err := Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, write := range []func() error{
		func() error { return reopened.CanRecord(ctx) },
		func() error { return reopened.Append(ctx, decision(id)) },
		func() error { return reopened.AppendAdmitted(ctx, decision(id)) },
		func() error { return reopened.Refuse(ctx) },
	} {
		if err := write(); !errors.Is(err, ErrSealed) {
			t.Fatalf("sealed producer admitted: %v", err)
		}
	}
	got, err := reopened.Receipt(ctx)
	if err != nil || got != accepted {
		t.Fatal("sealed receipt changed")
	}
	if err := reopened.Replay(ctx, func(context.Context, []Record) error { t.Fatal("settled history replayed"); return nil }); err != nil {
		t.Fatal(err)
	}
	if sealed, err := reopened.Seal(ctx, accepted); err != nil || !sealed {
		t.Fatal("lost seal acknowledgement not idempotent")
	}
}
