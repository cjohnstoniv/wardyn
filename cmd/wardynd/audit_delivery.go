// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// startAuditDelivery starts the acknowledged webhook delivery loop when the
// sinks config asks for it (#1513), on the sweeper leader only so several
// replicas do not each send the whole trail. The loop reads the stored rows, so
// it is not on any write path. ParseSinks already refused a bad config at boot
// (buildAuditFanout), so an error here is logged and delivery stays off rather
// than failing a started server.
func startAuditDelivery(ctx context.Context, sinksJSON string, st store.PG, rec audit.Recorder, leader *db.SweeperLeader) {
	cfg, err := sinks.AcknowledgedWebhook([]byte(sinksJSON))
	if err != nil {
		slog.Error("wardynd: acknowledged audit delivery not started", slog.Any("err", err))
		return
	}
	if cfg == nil {
		return
	}
	a, err := sinks.NewAckedWebhook(*cfg, st, rec)
	if err != nil {
		slog.Error("wardynd: acknowledged audit delivery not started", slog.Any("err", err))
		return
	}
	leaderGo(ctx, leader, "audit.delivery", a.Run)
}

// auditDeliveryReporter adapts the store's checkpoint status to the
// api.Config.AuditDelivery callback. Nil unless acknowledged delivery is
// configured, so the wardyn_audit_delivery_* series are omitted rather than
// emitted empty. The status is read from the database, so any replica reports
// it, not only the leader running the loop.
func auditDeliveryReporter(sinksJSON string, st store.PG) func(context.Context) ([]store.AuditDeliveryStatus, error) {
	if cfg, err := sinks.AcknowledgedWebhook([]byte(sinksJSON)); err != nil || cfg == nil {
		return nil
	}
	return st.AuditDeliveryStatuses
}
