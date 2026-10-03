// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The run masking manifest's side of the API (ha-l2.0): committing it at
// dispatch, and the five doors that refuse while a run is not covered by it.
//
// A door that relays or persists a run's output masks it against the registry,
// and a registry that does not hold the run's values passes bytes through. After
// a restart, or on a second replica, that is exactly what an empty registry is.
// maskmanifest.Covered says whether this process can prove the run's corpus
// whole; each door asks it, in Postgres, at admission, and refuses when it is
// false. An in-flight consumer asks again at each beat and ends on a fence.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// maskScopeGlobalsOnly is the mask_scope an audit row of an uncovered run
	// carries: only the process-wide corpus masked it.
	maskScopeGlobalsOnly = "globals_only"
	// maskCheckEvery is how often an in-flight consumer (an attach, an SSH
	// shell, the exec relay, an upload) re-reads its run's fence.
	maskCheckEvery = 2 * time.Second
	// maskCheckTimeout bounds one admission read: a Postgres that does not
	// answer means "not covered", never a hang.
	maskCheckTimeout = 5 * time.Second

	maskStateSentence = "this server cannot prove this run's secrets are masked right now, so it refuses to relay or keep the run's output"
)

// errMaskUncovered ends an upload whose run stopped being covered mid-stream.
var errMaskUncovered = errors.New("api: the run's masking manifest no longer covers it")

func (s *Server) maskCheckPeriod() time.Duration {
	if s.maskBeat > 0 {
		return s.maskBeat
	}
	return maskCheckEvery
}

// maskCovered reports whether runID's corpus is provably whole. A deployment
// that keeps no manifests (a nil Config.MaskManifests: tests, and nothing in
// production) gates nothing.
func (s *Server) maskCovered(ctx context.Context, runID uuid.UUID) bool {
	m := s.cfg.MaskManifests
	if m == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, maskCheckTimeout)
	defer cancel()
	return m.Covered(ctx, runID)
}

// maskGuard is the per-chunk check an in-flight consumer of runID carries: nil
// when no manifests are kept, else Covered re-read at most once per beat.
func (s *Server) maskGuard(runID uuid.UUID) func() bool {
	if s.cfg.MaskManifests == nil {
		return nil
	}
	return s.cfg.MaskManifests.Watch(runID, s.maskCheckPeriod())
}

// maskRefusal is the denial of door for an uncovered runID.
func maskRefusal(runID uuid.UUID, door string) authz.Decision {
	return authz.Deny(authz.ReasonMaskStateUnavailable, door, maskStateSentence).
		OnRun(runID).With("mask_scope", maskScopeGlobalsOnly)
}

// refuseUncovered answers 503 mask_state_unavailable, with its denied audit
// row, when runID is not covered, and reports whether it did. door names the
// door in the row's target.
func (s *Server) refuseUncovered(w http.ResponseWriter, r *http.Request, runID uuid.UUID, door string) bool {
	if s.maskCovered(r.Context(), runID) {
		return false
	}
	return s.refuse(w, r, maskRefusal(runID, door))
}

// auditUncovered records the denied row of a door that has no HTTP response
// (the SSH shell), as the actor that asked.
func (s *Server) auditUncovered(ctx context.Context, runID uuid.UUID, actor types.ActorType, subject, door string) {
	s.recordAudit(ctx, s.refusalEvent(ctx, actor, subject, "", maskRefusal(runID, door)))
}

// holdMaskFence ends an in-flight consumer of runID when the run stops being
// covered: it re-reads at each beat until ctx ends and calls end once on the
// first miss. The returned func reports whether it ended the consumer; for a
// deployment keeping no manifests it is always false.
func (s *Server) holdMaskFence(ctx context.Context, runID uuid.UUID, end func()) func() bool {
	if s.cfg.MaskManifests == nil {
		return func() bool { return false }
	}
	var fenced atomic.Bool
	go func() {
		t := time.NewTicker(s.maskCheckPeriod())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !s.maskCovered(ctx, runID) {
					if ctx.Err() != nil {
						return
					}
					fenced.Store(true)
					end()
					return
				}
			}
		}
	}()
	return fenced.Load
}

// beginMaskManifest creates the dispatching run's masking manifest before any
// value is resolved. false means the run has been failed.
func (s *Server) beginMaskManifest(ctx context.Context, run types.AgentRun) bool {
	m := s.cfg.MaskManifests
	if m == nil {
		return true
	}
	if err := m.Start(ctx, run.ID, runIdentitySubject(ctx, run.CreatedBy)); err != nil {
		return s.failMaskManifest(ctx, run, "start", err)
	}
	return true
}

// completeMaskManifest marks the run's manifest complete, once every value the
// run receives at dispatch is committed and before the sandbox can see one.
// false means the run has been failed.
func (s *Server) completeMaskManifest(ctx context.Context, run types.AgentRun) bool {
	m := s.cfg.MaskManifests
	if m == nil {
		return true
	}
	if err := m.Complete(ctx, run.ID); err != nil {
		return s.failMaskManifest(ctx, run, "complete", err)
	}
	return true
}

func (s *Server) failMaskManifest(ctx context.Context, run types.AgentRun, step string, err error) bool {
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
		run.ID.String(), "failure", mustJSON(map[string]any{
			"note": "the run's masking manifest could not be committed (" + step + "); refusing to launch a run whose secrets cannot be masked after a restart",
		})))
	s.failAndRevoke(ctx, run.ID, types.RunStarting, "the run's masking manifest could not be committed")
	return false
}

// maskDispatchValue registers values with the run's mask registry and commits
// them to its manifest, before the caller hands them to the run. An error means
// they are not on record: the caller must not hand them out.
func (s *Server) maskDispatchValue(ctx context.Context, runID uuid.UUID, values ...[]byte) error {
	for _, v := range values {
		s.cfg.MaskRegistry.Add(runID, v)
	}
	if m := s.cfg.MaskManifests; m != nil {
		return m.Append(ctx, runID, values...)
	}
	return nil
}

// maskMintedValue is maskDispatchValue for a credential minted after dispatch
// (a run token's renewal, widening or resume). A run with no manifest, one
// dispatched before manifests existed, stays honestly uncovered and is not an
// error: refusing its token would break a run that is running.
func (s *Server) maskMintedValue(ctx context.Context, runID uuid.UUID, values ...[]byte) error {
	err := s.maskDispatchValue(ctx, runID, values...)
	if errors.Is(err, maskmanifest.ErrNoManifest) {
		return nil
	}
	return err
}

// loadMaskManifests loads the manifest of every live run, so coverage is back
// at boot rather than at the first door that asks (ReconcileOnBoot).
func (s *Server) loadMaskManifests(ctx context.Context) error {
	m := s.cfg.MaskManifests
	if m == nil || s.cfg.Store == nil {
		return nil
	}
	runs, err := s.cfg.Store.ListRuns(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if !isTerminalRunState(run.State) {
			m.Covered(ctx, run.ID)
		}
	}
	return nil
}

// forgetMaskManifest drops this process's copy of an evicted run's manifest.
func (s *Server) forgetMaskManifest(runID uuid.UUID) {
	if s.cfg.MaskManifests != nil {
		s.cfg.MaskManifests.ForgetCache(runID)
	}
}

// maskFencedReason is the reason a session.detach row records for a session a
// fence or a lost manifest ended.
const maskFencedReason = "mask state unavailable"

// endAttachOnMaskFence ends the web attach on c when its run stops being
// covered, with a close frame (1013, try again later) the console can read, and
// reports whether it did.
func (s *Server) endAttachOnMaskFence(ctx context.Context, runID uuid.UUID, c *websocket.Conn, cancel context.CancelFunc) func() bool {
	return s.holdMaskFence(ctx, runID, func() {
		// Close performs the close handshake, so it runs apart from the beat;
		// cancel is the backstop when the peer never answers.
		go func() {
			_ = c.Close(websocket.StatusTryAgainLater, string(authz.ReasonMaskStateUnavailable))
			cancel()
		}()
	})
}

// endSSHOnMaskFence ends the SSH shell on channel when its run stops being
// covered, and reports whether it did. The cancel is the act and the stderr
// line a courtesy, so the cancel is on a timer the write cannot outlive (see
// the displace closure in bridgeSSHShell).
func (s *Server) endSSHOnMaskFence(ctx context.Context, runID uuid.UUID, channel ssh.Channel, cancel context.CancelFunc) func() bool {
	return s.holdMaskFence(ctx, runID, func() {
		go func() {
			late := time.AfterFunc(sshDisplaceGrace, cancel)
			_, _ = fmt.Fprintln(channel.Stderr(), "wardyn: this server can no longer prove this run's secrets are masked; the shell is closed")
			late.Stop()
			cancel()
		}()
	})
}
