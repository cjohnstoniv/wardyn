// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/notify"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mountApprovalNotifyRoutes registers GET /approval-notify/status. Called with securityOps: the
// security tier decides approvals, so it is the tier that must learn an announcement is not arriving.
func (s *Server) mountApprovalNotifyRoutes(securityOps chi.Router) {
	securityOps.Get("/approval-notify/status", s.handleApprovalNotifyStatus)
}

// errClassRe is the shape of a stored failure class ("http_status:503", "timeout"). The worker only
// ever writes such a value; the guard keeps anything else out of the response.
var errClassRe = regexp.MustCompile(`^[a-z0-9_:]{1,64}$`)

type approvalNotifyChannelStatus struct {
	ID              string     `json:"id"`
	Type            string     `json:"type"`
	DestinationHost string     `json:"destination_host"`
	LastSuccessAt   *time.Time `json:"last_success_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	LastErrorAt     *time.Time `json:"last_error_at,omitempty"`
	FailedLastHour  int        `json:"failed_last_hour"`
}

type approvalNotifyStatusResponse struct {
	Channels []approvalNotifyChannelStatus `json:"channels"`
}

// approvalNotifyStats reads each configured channel's delivery history. ok is false when
// notifications are off or the backend cannot read the outbox.
func (s *Server) approvalNotifyStats(ctx context.Context) (rows []approvalNotifyChannelStatus, ok bool, err error) {
	infos := notify.ChannelInfos()
	rd, capable := s.cfg.Approvals.(store.ApprovalNotifyReader)
	if len(infos) == 0 || !capable {
		return nil, false, nil
	}
	stats, err := rd.ApprovalNotifyChannelStats(ctx, s.cfg.Now())
	if err != nil {
		return nil, false, err
	}
	byChannel := make(map[string]types.ApprovalNotifyChannelStat, len(stats))
	for _, st := range stats {
		byChannel[st.Channel] = st
	}
	rows = make([]approvalNotifyChannelStatus, 0, len(infos))
	for _, in := range infos {
		st := byChannel[in.ID]
		row := approvalNotifyChannelStatus{
			ID: in.ID, Type: in.Type, DestinationHost: in.Host,
			LastSuccessAt: st.LastSentAt, LastErrorAt: st.LastErrorAt, FailedLastHour: st.FailedLastHour,
		}
		if errClassRe.MatchString(st.LastError) {
			row.LastError = st.LastError
		}
		rows = append(rows, row)
	}
	return rows, true, nil
}

// handleApprovalNotifyStatus (securityOps) reports each configured channel's id, type, destination
// HOST, last success, last error class and time, and dead rows in the last hour. Never a URL, a
// query, userinfo or a secret: the host is parsed from the configured URL, and a chat webhook's
// credential lives in its path. Unconfigured, it answers an empty channel list.
func (s *Server) handleApprovalNotifyStatus(w http.ResponseWriter, r *http.Request) {
	rows, _, err := s.approvalNotifyStats(r.Context())
	if err != nil {
		writeServerError(w, r, "read approval notify status", err)
		return
	}
	if rows == nil {
		rows = []approvalNotifyChannelStatus{}
	}
	writeJSON(w, http.StatusOK, approvalNotifyStatusResponse{Channels: rows})
}

// approvalNotifyCheck is the non-blocking approval_notify setup row. Present only when notifications
// are configured: warn when any channel has a dead row in the last hour, otherwise ok. An unreadable
// outbox drops the row rather than failing /setup/status, which nothing else here depends on.
func (s *Server) approvalNotifyCheck(ctx context.Context) (SetupCheck, bool) {
	rows, ok, err := s.approvalNotifyStats(ctx)
	if err != nil {
		slog.WarnContext(ctx, "setup status: approval notify check skipped", slog.Any("error", err))
		return SetupCheck{}, false
	}
	if !ok {
		return SetupCheck{}, false
	}
	failed := 0
	for _, row := range rows {
		failed += row.FailedLastHour
	}
	chk := SetupCheck{ID: "approval_notify", Label: "Approval notifications", Status: "ok", Detail: "No channel has failed in the last hour."}
	if failed > 0 {
		chk.Status = "warn"
		chk.Detail = fmt.Sprintf("%d approval notifications failed in the last hour. The approvals still wait in the console.", failed)
		chk.Fix = "See Settings → Approval notifications for each channel's last error."
	}
	return chk, true
}
