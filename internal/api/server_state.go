// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Server is the control-plane HTTP server. It is safe for concurrent use.
type Server struct {
	svcGrant        serviceGrantRuntime
	runnerStreams   runnerStreamState
	gatewayIdentity gatewayIdentityState
	cfg             Config
	router          chi.Router
	// metrics holds the /metrics scrape counters (see metrics.go). Zero value is ready to use.
	metrics metrics
	// recordingChainWalk lets one recording range check (recording_chain.go) walk the audit log at a time on this replica:
	// the walk hashes the org's whole log over the run's lifetime, so a second check is refused, not queued. Zero value is ready to use.
	recordingChainWalk sync.Mutex
	profileSynth       atomic.Int32 // profile syntheses running on this replica (profile.go); each holds a run's audit trail in memory
	// sshUnbound is set by MarkSSHGatewayUnbound when the SSH gateway could not bind at boot; /healthz then omits ssh.
	sshUnbound atomic.Bool
	// viaOrg is the organisation's via_org egress state (via_org_streams.go); fleet is the 15-second capacity snapshot behind the /metrics capacity gauges (metrics_fleet.go).
	viaOrg viaOrgState
	fleet  fleetSnapshot
	// revokedEndRetry and exitRetry space out the queue and exit sweep retries (runner_revoke.go, runner_exits.go); eventRefusals bounds each runner's workload events on this replica and keeps, until it ends, each refusal window the store reported full, so rate_limited refusals skip the store (runner_event_refuse.go).
	revokedEndRetry revokedEndRetry
	exitRetry       runnerExitRetry
	eventRefusals   runnerEventRefusals
	// inventoryHoldAudit spaces one runner's runner.inventory.hold rows (runner_state.go); privIPRefusals caches the private_ip_refusals row's audit read (setup_private_ip_refusals.go).
	inventoryHoldAudit inventoryHoldAudit
	privIPRefusals     privateIPRefusalCache
	// capRowsScanned counts capability-grant rows compared inside capBatch (see capabilities.go). It is
	// INSTRUMENTATION, read by nothing on any request path: the per-request cost of the capability seam is
	// chosen partly by the member's own request body (spec.AllowedDomains), so "how much work did one request
	// buy" is a claim that has to be assertable rather than asserted — capability_batch_test.go's growth law
	// reads it. One atomic add per row already being compared. Zero value is ready to use.
	capRowsScanned atomic.Int64
	// siteConfigSeed is the boot-time WARDYN_SITE_CONFIG_SEED_FILE (nil when
	// unset), kept for the /setup/status row (site_config_seed.go).
	siteConfigSeed *SiteConfigSeed
	// locks is the in-process fallback for the cross-replica locks (locks.go).
	// The audit chain verify sweep, the site-config and capability-enforcement
	// writers, the per-run operation lock and the two refresh single-flights
	// all take theirs there. Zero value is ready to use.
	locks lockState
	// lastTouch debounces the decision-ingest TouchRun UPDATEs per run (see
	// shouldTouch in internal.go). Zero value is ready to use.
	lastTouchMu sync.Mutex
	lastTouch   map[uuid.UUID]time.Time
	// keepaliveEvery overrides attachKeepaliveInterval for THIS server only. It
	// exists so a test can watch the keepalive tick without waiting 30 real
	// seconds; nothing sets it in production and the zero value means "use the
	// constant" (attachKeepaliveEvery). Per-server rather than a package var so
	// two tests running side by side cannot race on it.
	keepaliveEvery time.Duration
	// pingEvery overrides attachPingInterval for THIS server only (the liveness
	// probe on an otherwise-idle attach socket) — same reason and same
	// per-server shape as keepaliveEvery above: a test drives a dead-peer holder
	// on a millisecond clock instead of the real 30s budget.
	pingEvery     time.Duration
	authorityTest attachAuthorityTimings // per-server test overrides, the same shape as pingEvery
	// localAttachSubs (localAttachSubFor) and localCovered (promotionCovered) replace a local attach's substrate and corpus proof; queueRace runs at each queueRace* point (queueRaceAt). Tests only.
	localAttachSubs func(id uuid.UUID) localAttachSub
	localCovered    func(runID uuid.UUID) bool
	queueRace       func(point string)
	macWindowHost   // this replica's open attended windows and the tests' seams for them (run_mac_window.go)
	// pauseLimit overrides attachWriteTimeout as the bound on an unresumed output pause for THIS server only (tests).
	pauseLimit time.Duration
	// attachPrepare overrides attachPrepareTimeout for THIS server only (tests):
	// the bound on one Runner.Attach.
	attachPrepare time.Duration
	// maskBeat overrides maskCheckEvery for THIS server only (tests): how often
	// an in-flight consumer re-reads its run's fence.
	maskBeat time.Duration
	// runOutputDrainWaitOverride and runOutputRetryBaseOverride shrink the
	// output finaliser's drain barrier and retry backoff for a test; zero uses
	// runOutputDrainWait and runOutputRetryBase (run_output_final.go).
	runOutputDrainWaitOverride, runOutputRetryBaseOverride time.Duration
	// runOutputRecoverWaitOverride shrinks runOutputRecoverWait for a test.
	runOutputRecoverWaitOverride time.Duration
	// paneSnapshotTimeoutOverride shrinks the pane snapshot's bound for a test;
	// zero uses paneSnapshotTimeout (run_output_snapshot.go).
	paneSnapshotTimeoutOverride time.Duration
	// refRuleset caches the ONE outbound GitHub call the setup checklist makes,
	// so polling /setup/status (which the wizard does) cannot turn into a
	// per-poll API call or a rate-limit. Zero value is ready to use.
	// substrateProbe is this replica's cached probe of the runner's substrate,
	// read by /metrics and /setup/status (substrate_health.go).
	substrateProbe substrateProbeCache
	refRulesetMu   sync.Mutex
	refRulesetAt   time.Time
	refRulesetRow  SetupCheck
	refRulesetShow bool
	// sshSessions counts concurrent SSH "session" channels (shell/exec/
	// subsystem) per run, enforcing Config.SSHMaxSessionsPerRun (sshgateway.go);
	// sshSyncSessions is the separate wardyn-sync count. Zero
	// value is ready to use. Process-local like lastTouch above — the same
	// single-replica topology every in-memory bound in this codebase already
	// assumes (see secretmask.Registry's residual in THREAT-MODEL.md).
	sshSessionsMu   sync.Mutex
	sshSessions     map[uuid.UUID]int
	sshSyncSessions map[uuid.UUID]int
	// sshSyncOpen is the directory and open time of each live wardyn-sync
	// channel, for GET /runs/{id}'s sync.open. Guarded by sshSessionsMu.
	sshSyncOpen map[uuid.UUID][]*runSyncOpen
	// builds tracks per-workspace image builds (the wizard's Build step). Zero value is ready to use.
	builds buildTracker
	// attachHolders tracks who currently holds each run's SHARED tmux PTY, so a
	// second client can be admitted read-only instead of silently competing for
	// the same terminal (see attach_holder.go). Process-local like sshSessions
	// and lastTouch above, and correct for the same reason: replicas>1 is
	// refused by construction (deployment.yaml). Zero value is ready to use.
	attachHolders attachHolderRegistry
	// creates lets a kill cancel a STARTING run's CreateSandbox, and runBuilds its image build (runs_create_cancel.go).
	creates, runBuilds inflightCreates
	// queue is the pool queue's in-process state: kick coalescing and the gauge snapshot (metrics_queue.go).
	queue runQueueState
	// replica is this server's identity when no LiveBus names one (replicaName).
	replica         string
	replicaOnce     sync.Once
	leaseKeeperOnce sync.Once       // starts attachLeaseKeeper (attach_lease.go)
	runEvents       runEventHub     // each run's lifecycle event ring (run_events.go)
	leaseDeferred   sync.Map        // see deferRenewedLease
	execOutputs     execOutputTails // each non-interactive run's output tail (run_output.go)
	uiGatewayState                  // the UI gateway's process state (server_uigateway.go)
	doorRefusals    doorRefusals    // the decision door's once-per-run refusal row (internal_live_run.go)
	// desktopRecs are the live desktop recordings and each run's pause (uigateway_record.go).
	desktopRecs desktopRecordings
	sensorBeat  sensorBeatState // the sensor heartbeat read's cache (healthz_sensor_beat.go)
	// attachIgnoredSeen holds the sub+person pairs whose sub_known person.attach row this process already
	// wrote (people.go attachIgnored); ponytail: per process, so a restart or another replica writes it
	// once more. A durable mark on the person row would need a migration.
	attachIgnoredSeen sync.Map
	// authFailedLimiter rate-bounds the auth.fail audit emit (see
	// adminAuth/auditAuthFailed in http.go) so a scanner cannot flood the
	// append-only log. Zero value is ready to use.
	authFailedLimiter authFailedLimiter
	// sshKeyLimiters rate-bounds the ssh.authenticate refusal rows that name a registered key, per key.
	sshKeyLimiters sshKeyLimiters
	// authFailedStreak is the open run of identical consecutive auth.fail rows
	// the coalescer is folding (see coalesceAuthFailed, http.go). Process-local
	// like the bounds above. Zero value is ready to use.
	authFailedStreakMu sync.Mutex
	authFailedStreak   *authFailedStreak
	// identityExpiredSeen is the per-run once-guard behind run.identity.expire
	// (claimIdentityExpired, http.go): a run whose identity has expired keeps
	// calling /internal/* and 401ing, so the row has to be emitted once per run
	// rather than once per request, or the audit log floods.
	// It is CLAIMED BEFORE the run read, so a repeat refusal costs no store read
	// either, and bounded at 4096 entries exactly like lastTouch. In memory and
	// process-local, like lastTouch/sshSessions above: a restart may re-emit once
	// per run, which is the acceptable end of the trade.
	identityExpiredMu   sync.Mutex
	identityExpiredSeen map[uuid.UUID]bool
	// dirLimiter rate-bounds GET /access/directory/search PER PRINCIPAL — it is
	// hit once per keystroke, and each miss is an upstream Graph call
	// (directory_search.go). Zero value is ready to use.
	dirLimiter principalLimiter
	// branchLimiter rate-bounds GET /workspaces/{id}/branches per person (workspace_branches.go): each
	// call can use a forge credential. memberDriveLimiter bounds member drive changes (member_drives.go).
	branchLimiter, memberDriveLimiter principalLimiter
	// backchannelLimiter bounds the logout tokens that fail to verify on POST /auth/backchannel-logout, and
	// frontchannelLimiter the requests that cut nothing on GET /auth/frontchannel-logout (signin_denied.go).
	backchannelLimiter, frontchannelLimiter principalLimiter
	// preflightLimiter rate-bounds POST /runs/preflight per person
	// (preflight.go); nil when Config.PreflightRatePerMin is 0 (off).
	preflightLimiter     *principalLimiter
	policyPreviewLimiter *principalLimiter
	deviceRouteState                 // the device routes' process state (server_devices.go)
	supportState                     // the open support grants this replica knows, for the audit window (support_grant.go)
	cliSignInState                   // CLI sign-in's rate limiters (cli_sign_in.go)
	scimState                        // the SCIM routes' rate limiters (scim_auth.go)
	runLeaseState                    // the run lease sweep's process state (run_lease_server.go)
	runEndState                      // the end loops' process state (run_end_loops.go)
	pause                pauseClocks // the pause sweep's process-local state (run_pause.go)
	// activity is the CPU signal's last read, for the idle detection row (run_activity.go).
	activity activitySignal
	// ssoRefreshMu guards ssoRefreshSpent, which the control-plane AWS SSO
	// refresher owns (awssso_refresh.go). The refresh is single-flight per owner
	// through a cross-replica lock (locks.go) that encloses re-read -> expiry
	// check -> CreateToken -> Put, so two dispatches of the same principal, on
	// this or another replica, cannot both redeem one rotating refresh token
	// (the second re-reads inside the lock and finds it already renewed).
	// ssoRefreshSpent records the fingerprints of refresh tokens the OIDC
	// endpoint has already told us are gone, keyed to the TOKEN rather than the
	// credential, so a later capture is never pre-marked dead. Process-local
	// like sshSessions and lastTouch above — but, unlike them, a CACHE rather
	// than the source of truth: it is write-through and read-once-memoized
	// against store.AWSSSOSpentTokenStore (Postgres), so a restart no longer
	// re-grades an already-spent credential `live` (#149) — the first read of a
	// spent fingerprint after a restart costs one best-effort row read, and
	// every read after that (in THIS process) costs nothing. Only spent marks
	// are memoized (#2138).
	// ponytail: ssoRefreshSpent grows one small entry per spent token per daemon
	// lifetime — bound it only if that ever stops being negligible.
	ssoRefreshMu    sync.Mutex
	ssoRefreshSpent map[string]bool
	// ssoRefreshRefused is the non-spent counterpart (awssso_refused.go): the
	// refresh tokens AWS last refused to renew with a re-auth code, to the code
	// and description it answered, also under ssoRefreshMu. Used only by a store
	// with no store.AWSSSORefusedTokenStore; with one, the marks are shared there.
	// Bounded by awsSSORefusedMax.
	ssoRefreshRefused map[string]string
	// ssoRenew is the background renewer's per-credential schedule (awssso_renewer.go).
	ssoRenew awsSSORenewSchedule
	// adoEntraTokens reuses a minted Azure DevOps access token across the
	// per-host grants of one run (injection_ado.go), so a sidecar's boot does
	// not rotate one person's refresh token once per host.
	adoEntraTokens adoEntraAccessCache
	// adoSignInEnds counts each person's disconnects and erases, so a run
	// token created while one ran revokes itself (ado_pat_console.go).
	adoSignInEnds adoSignInEnds
	// adoPATs replaces the sign-in configuration's own vssps client (ADOEntraConfig.patClient) for a
	// `minted_pat` run's creates and revokes; set by tests only. adoRunPATs holds each run's current
	// token in memory (ado_run_pat_cache.go).
	adoPATs    adoPATClient
	adoRunPATs adoRunPATCache
	// bg tracks every goroutine spawned through goBackground — work detached
	// from a request so the answering call does not wait for it
	// (finishHarnessLoginLaunch, killTeardownTail's supersede caller).
	// http.Server (cmd/wardynd/boot_serve.go) only waits for in-flight
	// HANDLERS to return; these goroutines outlive their handler by design, so
	// Shutdown alone would let a SIGTERM cut one off mid-teardown — a run left
	// KILLED with its sandbox still up, its credentials unrevoked and no
	// run.kill row. See WaitBackground, which cmd/wardynd calls after
	// httpSrv.Shutdown so an orderly stop gives this work its own bounded
	// window to finish. Zero value is ready to use.
	bg sync.WaitGroup
	// bgWaitBudget overrides backgroundShutdownBudget for THIS server only,
	// same shape as keepaliveEvery/pingEvery above: a test proving WaitBackground
	// actually gives up at its bound needs to do so in milliseconds, not the
	// real ~35s budget. Zero (the default) means "use backgroundShutdownBudget".
	// See background.go; signInCaptureKillGrace is ssotoken.go's (tests set 0).
	bgWaitBudget, signInCaptureKillGrace time.Duration
}
