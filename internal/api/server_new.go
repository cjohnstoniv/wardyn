// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"time"
)

// New constructs a Server and builds its router. It does not start listening.
func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RunnerTarget == "" {
		cfg.RunnerTarget = "docker"
	}
	if cfg.SSHRoleTTL <= 0 {
		cfg.SSHRoleTTL = defaultSSHRoleTTL
	}
	if cfg.SSHMaxSessionsPerRun <= 0 {
		cfg.SSHMaxSessionsPerRun = defaultSSHSessionsPerRun
	}
	if cfg.UISessionTTL <= 0 {
		cfg.UISessionTTL = defaultUISessionTTL
	}
	if cfg.RunOutputTailBytes <= 0 {
		cfg.RunOutputTailBytes = defaultRunOutputTailBytes
	}
	if cfg.ExecOutputTailTTL <= 0 {
		cfg.ExecOutputTailTTL = defaultExecOutputTailTTL
	}
	if cfg.BaseCtx == nil {
		cfg.BaseCtx = context.Background()
	}
	s := &Server{cfg: cfg, signInCaptureKillGrace: signInCaptureKillGrace,
		branchLimiter:      principalLimiter{rate: branchListRatePerSec, burst: branchListBurst, max: branchListMaxPeople},
		backchannelLimiter: principalLimiter{rate: backchannelLogoutRatePerSec, burst: backchannelLogoutBurst, max: 1},
		deviceRouteState:   newDeviceRouteState(),
	}
	s.scimState = newSCIMState()
	s.defaultRunnerHooks()
	if cfg.PreflightRatePerMin > 0 {
		s.preflightLimiter = &principalLimiter{rate: float64(cfg.PreflightRatePerMin) / 60, burst: preflightBurst, max: preflightLimiterMaxPeople}
	}
	if cfg.PolicyPreviewRatePerMin > 0 {
		s.policyPreviewLimiter = &principalLimiter{rate: float64(cfg.PolicyPreviewRatePerMin) / 60, burst: policyPreviewBurst, max: preflightLimiterMaxPeople}
	}
	s.router = s.routes()
	s.registerLiveBus()
	// drain the durable audit-fallback spool back into the store once it
	// recovers, so a PG outage no longer leaves spooled events permanently invisible
	// to /audit and `wardyn audit`. Uses BaseCtx (daemon lifetime) so it survives
	// individual requests and stops on shutdown. No-op unless both are wired.
	if cfg.AuditSpool != nil && cfg.AuditDrainRecorder != nil {
		go cfg.AuditSpool.StartDrain(cfg.BaseCtx, cfg.AuditDrainRecorder, auditSpoolDrainInterval, auditSpoolDrainBatch)
	}
	return s
}
