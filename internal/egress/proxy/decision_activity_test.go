// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runneractivity"
	"github.com/google/uuid"
)

func TestLocalDecisionActivityUsesAdmissionEpochOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	id := uuid.New()
	ctx := context.Background()
	sink, err := newLocalDecisionSink(dir, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.close(ctx)
	old, next := uuid.New(), uuid.New()
	if err := runneractivity.Publish(dir, old); err != nil {
		t.Fatal(err)
	}
	ticket, err := sink.reserve(ctx, spoolRecordEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := runneractivity.Publish(dir, next); err != nil {
		t.Fatal(err)
	}
	log := egress.DecisionLog{Request: egress.Request{RunID: id, Host: "allowed.test", Time: time.Now(), AuditReservation: ticket}, Decision: egress.Allow, RuleSource: "policy:allowed"}
	sink.emitLocal(log)
	marker, err := runneractivity.Read(dir)
	if err != nil || marker.Epoch != old {
		t.Fatalf("reconnect granted old request activity: %+v %v", marker, err)
	}
	for _, source := range []string{ruleSourceApprovals, "credential:reauth-timeout"} {
		ticket, err = sink.reserve(ctx, spoolRecordEnvelope)
		if err != nil {
			t.Fatal(err)
		}
		log.Request.AuditReservation = ticket
		log.RuleSource = source
		if source == "credential:reauth-timeout" {
			log.Decision = egress.Deny // the proxy only ever writes it as a deny
		}
		sink.emitLocal(log)
		same, err := runneractivity.Read(dir)
		if err != nil || same != marker {
			t.Fatal("synthetic waiting decision produced activity")
		}
	}
	log.Request.AuditReservation = uuid.Nil
	log.Decision, log.RuleSource = egress.Allow, "policy:allowed"
	sink.emitLocal(log)
	same, err := runneractivity.Read(dir)
	if err != nil || same != marker {
		t.Fatal("unadmitted/replayed decision produced activity")
	}
}

// The legacy local spool judges activity by the same ignore list: an allowed request to a listed host leaves
// the marker alone, and an unlisted one moves it.
func TestLocalDecisionActivity_IgnoredHost(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	id := uuid.New()
	ctx := context.Background()
	sink, err := newLocalDecisionSink(dir, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.close(ctx)
	sink.activityIgnore = []string{"mask.icloud.com"}
	epoch := uuid.New()
	if err := runneractivity.Publish(dir, epoch); err != nil {
		t.Fatal(err)
	}
	emit := func(host string) {
		ticket, err := sink.reserve(ctx, spoolRecordEnvelope)
		if err != nil {
			t.Fatal(err)
		}
		sink.emitLocal(egress.DecisionLog{Request: egress.Request{RunID: id, Host: host, Time: time.Now(), AuditReservation: ticket}, Decision: egress.Allow, RuleSource: "policy:allowed"})
	}
	emit("mask.icloud.com")
	if m, err := runneractivity.Read(dir); err == nil && m.Epoch != uuid.Nil {
		t.Fatalf("an allowed request to a listed host moved the marker: %+v", m)
	}
	emit("github.com")
	m, err := runneractivity.Read(dir)
	if err != nil || m.Epoch != epoch {
		t.Fatalf("an allowed request to an unlisted host: marker %+v %v, want epoch %s", m, err, epoch)
	}
}
