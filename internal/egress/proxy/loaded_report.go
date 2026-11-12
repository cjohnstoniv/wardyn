// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// loadedReportPath is the control plane's route a started proxy reports the config it loaded to.
const loadedReportPath = "/api/v1/internal/proxy/loaded"

// loadedReportAttempt bounds one report POST; the backoff between attempts doubles from
// loadedReportMinBackoff up to loadedReportMaxBackoff. Variables so a test can shorten them.
var (
	loadedReportAttempt    = 10 * time.Second
	loadedReportMinBackoff = time.Second
	loadedReportMaxBackoff = 30 * time.Second
)

// ConfigSHA256 is the lowercase hex SHA-256 of a proxy config's raw bytes. A driver takes it of
// the bytes it hands a proxy and the proxy of the bytes it loaded; the control plane compares the two.
func ConfigSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// LoadedReport is what a started proxy tells the control plane it loaded: ConfigSHA256 of the
// raw config bytes, its own version, and the deployment switches (by env name, as set) it
// applied after the load, which are not part of the run's policy.
type LoadedReport struct {
	ConfigSHA256 string            `json:"config_sha256"`
	ProxyVersion string            `json:"proxy_version"`
	Switches     map[string]string `json:"switches"`
}

// ReportLoaded posts r with the run token until the control plane answers 2xx (a report it holds
// counts), answers 404 or 405 (an older wardynd with no such route), or ctx ends. Call it once the
// proxy serves.
func (s *Server) ReportLoaded(ctx context.Context, r LoadedReport) { s.proxy.reportLoaded(ctx, r) }

// reportLoaded is ReportLoaded: one POST per attempt over p.localClient, retried with backoff.
func (p *Proxy) reportLoaded(ctx context.Context, r LoadedReport) {
	body, err := json.Marshal(r)
	if err != nil {
		slog.Error("wardyn-proxy: encode the loaded-config report", slog.Any("err", err))
		return
	}
	backoff := loadedReportMinBackoff
	for attempt := 1; ; attempt++ {
		status, err := p.postLoadedReport(ctx, body)
		if err == nil && status/100 == 2 {
			slog.Info("wardyn-proxy: reported the loaded config", slog.String("config_sha256", r.ConfigSHA256), slog.Int("attempt", attempt))
			return
		}
		if err == nil && (status == http.StatusNotFound || status == http.StatusMethodNotAllowed) {
			slog.Info("wardyn-proxy: the control plane has no loaded-config report route (an older wardynd); not reporting", slog.Int("status", status))
			return
		}
		if attempt == 1 {
			slog.Warn("wardyn-proxy: the loaded-config report was not accepted; retrying", slog.Int("status", status), slog.Any("err", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, loadedReportMaxBackoff)
	}
}

// postLoadedReport sends one report and answers the response status.
func (p *Proxy) postLoadedReport(ctx context.Context, body []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, loadedReportAttempt)
	defer cancel()
	resp, err := p.forwardToControlPlane(ctx, http.MethodPost, loadedReportPath, body, "application/json")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}
