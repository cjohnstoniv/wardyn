// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/sshsftp"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The walls a run's local_folders can set, in the order a record names them.
const (
	wallDenyRemote = "deny_remote"
	wallDenyPull   = "deny_pull"
	wallMaxPush    = "max_push_mib"
	wallMaxPull    = "max_pull_mib"
)

var localFolderWallNames = []string{wallDenyRemote, wallDenyPull, wallMaxPush, wallMaxPull}

const (
	localFolderWallsUnreadableMsg = "This run's folder walls could not be read. Try again."
	auditStepLocalFolderWalls     = "Store local folder walls"
	// maxLocalFolderCapMiB is the largest size cap a policy may set: 1 TiB.
	maxLocalFolderCapMiB = 1 << 20
)

var (
	errLocalFolderCap      = errors.New("a folder size limit of this run was reached")
	errLocalFolderDenyOver = errors.New("the run's folder ignore patterns cannot be served")
)

// localFolderWallSet reports whether src sets wall as run holds it: a bool wall src turns on, or src's positive cap
// equal to run's. run is the clamped walls, so a src cap that differs from it did not set it.
func localFolderWallSet(src, run *types.LocalFolderRules, wall string) bool {
	if src == nil || run == nil {
		return false
	}
	switch wall {
	case wallDenyRemote:
		return src.DenyRemote
	case wallDenyPull:
		return src.DenyPull
	case wallMaxPush:
		return src.MaxPushMiB > 0 && src.MaxPushMiB == run.MaxPushMiB
	case wallMaxPull:
		return src.MaxPullMiB > 0 && src.MaxPullMiB == run.MaxPullMiB
	}
	return false
}

// stepLocalFolderWalls refuses a run's local folder that its clamped local_folders walls off: deny_remote refuses a
// computer folder, and a `wardyn sync` folder unless the request places the run on the person's own computer;
// deny_pull refuses a computer folder that syncs back. It runs after stepComputerFolderDataOut, so local_folder is
// normalised, and it records each wall's layer (localFolderWallLayers) for the envelope.
func (s *Server) stepLocalFolderWalls(f *runFold, _ *foldRecorder) bool {
	walls := f.spec.LocalFolders
	if walls == nil {
		return true
	}
	layers, err := s.localFolderWallLayers(f.r.Context(), walls, f.ceiling, f.source.knobs)
	if err != nil {
		return !runServerError("read the Wardyn boundary", err).write(s, f.w, f.r)
	}
	f.source.localFolderLayers = layers
	lf := f.req.LocalFolder
	switch {
	case lf == nil:
		return true
	case walls.DenyRemote && lf.RunnerID != nil:
		return !localFolderWalledRefusal(layers, wallDenyRemote, "").write(s, f.w, f.r)
	case walls.DenyRemote && f.req.Placement != client.PlacementLocal:
		return !localFolderWalledRefusal(layers, wallDenyRemote, localFolderPlaceHint).write(s, f.w, f.r)
	case walls.DenyPull && lf.RunnerID != nil && lf.Direction == types.ComputerFolderBoth:
		return !localFolderWalledRefusal(layers, wallDenyPull, "").write(s, f.w, f.r)
	}
	return true
}

// localFolderPlaceHint ends a `wardyn sync` folder's deny_remote refusal.
const localFolderPlaceHint = "; pass --placement local to keep this run on your own computer"

// localFolderWhat is what a refusal under wall walls off: deny_remote's or deny_pull's.
func localFolderWhat(wall string) string {
	if wall == wallDenyPull {
		return "bringing the sandbox's changes back to your computer"
	}
	return "a folder from your computer for a run on another machine"
}

// localFolderWalledRefusal is the 422 for a wall that refuses a run at create: what it walls off and its layer.
func localFolderWalledRefusal(layers map[string]types.LocalFolderWallLayer, wall, hint string) *runRefusal {
	return runError(http.StatusUnprocessableEntity, reasonLocalFolderWalled, localFolderWalledText(wall, layers[wall])+hint)
}

// localFolderWalledText is a wall's refusal: "local_folder: <what> is walled off by <its layer>".
func localFolderWalledText(wall string, l types.LocalFolderWallLayer) string {
	return "local_folder: " + localFolderWhat(wall) + " is walled off by " + localFolderLayer(l.Layer, l.Name, l.Locked)
}

// localFolderWallLayers is the layer of each wall the run's clamped walls set: the outermost layer whose own
// local_folders sets it. That is the boundary in hard_limits, else the run's unit ceiling, else the run's own policy.
// Locked is the knob's lock (knobs). A boundary that cannot be read is an error.
func (s *Server) localFolderWallLayers(ctx context.Context, run *types.LocalFolderRules, ceiling governanceCeiling, knobs knobProvenance) (map[string]types.LocalFolderWallLayer, error) {
	if run == nil {
		return nil, nil
	}
	b, err := s.governanceBoundary(ctx)
	if err != nil {
		return nil, err
	}
	var boundary, unit *types.LocalFolderRules
	if b.HardLimits() {
		boundary = b.Spec.LocalFolders
	}
	if ceiling.Unit != nil {
		unit = ceiling.Unit.Ceiling.LocalFolders
	}
	unitP, ownP := ceiling.provenance(), ceiling.ownProvenance()
	locked := knobs["local_folders"].Locked
	out := map[string]types.LocalFolderWallLayer{}
	for _, wall := range localFolderWallNames {
		if !localFolderWallSet(run, run, wall) {
			continue
		}
		l := types.LocalFolderWallLayer{Layer: ownP.Layer, Name: ownP.Name, Locked: locked}
		switch {
		case localFolderWallSet(boundary, run, wall):
			l.Layer, l.Name = policyref.LayerBoundary, ""
		case localFolderWallSet(unit, run, wall):
			l.Layer, l.Name = unitP.Layer, unitP.Name
		}
		out[wall] = l
	}
	return out, nil
}

// stepLocalFolderWallsPlaced refuses deny_remote with a `wardyn sync` folder unless every runner target placement
// holds is the run's person's own and none is remote-hosted, so the run never waits on a seed the sync channel would
// refuse. It runs after stepPlacement, at create, preflight, preview and the queued start.
func (s *Server) stepLocalFolderWallsPlaced(f *runFold, _ *foldRecorder) bool {
	walls, lf := f.spec.LocalFolders, f.req.LocalFolder
	if walls == nil || !walls.DenyRemote || lf == nil || lf.RunnerID != nil {
		return true
	}
	own, err := s.targetsOwnedBy(f.r.Context(), f.place, actorPrincipal(f.r))
	if err != nil {
		return !runServerError("read the run's runners", err).write(s, f.w, f.r)
	}
	if own {
		return true
	}
	return !localFolderWalledRefusal(f.source.localFolderLayers, wallDenyRemote, localFolderPlaceHint).write(s, f.w, f.r)
}

func actorPrincipal(r *http.Request) string {
	_, principal := actorFromRequest(r)
	return principal
}

// targetsOwnedBy reports whether p places the run only on runners person-owned by principal: a pool that decided a
// placement other than local, or any remote-hosted or other target, is false. An unpooled run has no targets here; the
// request's placement was checked by stepLocalFolderWalls and the sync channel checks the runner it landed on.
func (s *Server) targetsOwnedBy(ctx context.Context, p runPlacement, principal string) (bool, error) {
	if p.pooled && p.placement != types.PlacementLocal {
		return false, nil
	}
	rs, _ := s.cfg.Store.(runnerGetter)
	for _, t := range p.eligible {
		if t.runnerID == nil || t.ownerKind != types.RunnerOwnerPerson || rs == nil {
			return false, nil
		}
		rn, err := rs.GetRunner(ctx, *t.runnerID)
		if err != nil {
			return false, err
		}
		if rn.OwnerKind != types.RunnerOwnerPerson || rn.Owner != principal {
			return false, nil
		}
	}
	return true, nil
}

// localFolderLayer names a wall's layer in a sentence, in the console's words (its layer chips): the Wardyn boundary
// for the boundary and the deployment layer, a unit by name, or this run's policy for every other layer, with
// " (locked)" when the knob is locked. Its arguments are one wall's LocalFolderWallLayer fields.
func localFolderLayer(layer, name string, locked bool) string {
	out := "this run's policy"
	switch {
	case layer == policyref.LayerBoundary || layer == policyref.LayerDeployment:
		out = "the Wardyn boundary"
	case layer == policyref.LayerUnit && name != "":
		out = "the " + name + " unit"
	}
	if locked {
		out += " (locked)"
	}
	return out
}

func wallLayerText(l types.LocalFolderWallLayer) string {
	return localFolderLayer(l.Layer, l.Name, l.Locked)
}

// localFolderBackText is the console's form of the deny_pull sentence, with no local_folder: prefix.
func localFolderBackText(rec types.LocalFolderWallsRecord) string {
	return "Bringing the sandbox's changes back to your computer is walled off by " + wallLayerText(rec.Layers[wallDenyPull]) + "."
}

// localFolderPullCLIText is the sentence `wardyn sync --pull` exits with under deny_pull, before it connects.
func localFolderPullCLIText(rec types.LocalFolderWallsRecord) string {
	return localFolderWalledText(wallDenyPull, rec.Layers[wallDenyPull]) + "; run wardyn sync without --pull to send files to the run"
}

// localFolderCapText is the sentence a sync session ends with at a size cap, with no local_folder: prefix.
func localFolderCapText(limit int64, way string, l types.LocalFolderWallLayer) string {
	what := "sending in"
	if way == "pull" {
		what = "bringing back"
	}
	return "This sync session reached its " + consoleBytes(limit) + " limit for " + what + ", set by " + wallLayerText(l)
}

// consoleBytes is the console's fmtBytes form of n bytes (ui/src/app/lib/format.ts).
func consoleBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1024/1024)
}

// auditLocalFolderDenied records one run.local_folder.deny row: the wall that refused and its layer.
func (s *Server) auditLocalFolderDenied(ctx context.Context, runID uuid.UUID, actorType types.ActorType, actor, wall string, l types.LocalFolderWallLayer) {
	s.recordAudit(ctx, s.auditEvent(&runID, actorType, actor, "run.local_folder.deny", runID.String(), "denied",
		mustJSON(map[string]any{"wall": wall, "layer": l.Layer, "name": l.Name, "locked": l.Locked})))
}

// localFolderPullGuard is sshSyncReplyGuard's shape for deny_pull under a data_out rule that lets content out: the
// first file-content reply ends the sync with the deny_pull sentence and one run.local_folder.deny row.
func (s *Server) localFolderPullGuard(runID uuid.UUID, principal string, channel ssh.Channel, out io.Reader, rec types.LocalFolderWallsRecord) io.Reader {
	l := rec.Layers[wallDenyPull]
	guard := &sshsftp.ReplyGuard{R: out, OnData: func() {
		s.auditLocalFolderDenied(s.cfg.BaseCtx, runID, types.ActorHuman, principal, wallDenyPull, l)
	}}
	return &syncStopOnData{r: guard, stop: func() {
		sendChannelError(channel, "wardyn: bringing the sandbox's changes back to your computer is walled off by "+wallLayerText(l)+", so `wardyn sync` only sends files to the run")
	}}
}

// folderWallsView is what the run's folder walls mean for the pass back, for the Folders from your computer rows.
type folderWallsView struct {
	BackWalled     *types.LocalFolderWallLayer `json:"back_walled,omitempty"`
	BackWalledText string                      `json:"back_walled_text,omitempty"`
	Unreadable     bool                        `json:"unreadable,omitempty"`
}

// folderWallsView is nil when the run's walls allow a pass back, the deny_pull layer and sentence under deny_pull,
// and unreadable when the walls could not be read.
func (s *Server) folderWallsView(ctx context.Context, runID uuid.UUID) *folderWallsView {
	rec, err := s.runLocalFolderWalls(ctx, runID)
	if err != nil {
		return &folderWallsView{Unreadable: true}
	}
	if rec.Walls == nil || !rec.Walls.DenyPull {
		return nil
	}
	l := rec.Layers[wallDenyPull]
	return &folderWallsView{BackWalled: &l, BackWalledText: localFolderBackText(rec)}
}

// folderPullWalled reports whether the run's walls refuse a pass back. A read error is returned, never "no walls".
func (s *Server) folderPullWalled(ctx context.Context, runID uuid.UUID) (bool, error) {
	rec, err := s.runLocalFolderWalls(ctx, runID)
	return err == nil && rec.Walls != nil && rec.Walls.DenyPull, err
}

// keepRunLocalFolderWalls stores the run's walls on its row before the run.policy.resolve row records them, the
// twin of keepRunDataOut. layers are the envelope's local_folder_layers; an envelope with none names src, the knob's
// layer, for every wall. nil walls, or a store that keeps none, writes nothing. A write error fails the run.
func (s *Server) keepRunLocalFolderWalls(ctx context.Context, run types.AgentRun, walls *types.LocalFolderRules, src policyref.Provenance, layers map[string]types.LocalFolderWallLayer, stopLease func()) bool {
	st, ok := s.cfg.Store.(store.RunLocalFolderWallsStore)
	if walls == nil || !ok {
		return true
	}
	if layers == nil {
		layers = map[string]types.LocalFolderWallLayer{}
		for _, wall := range localFolderWallNames {
			if localFolderWallSet(walls, walls, wall) {
				layers[wall] = types.LocalFolderWallLayer{Layer: src.Layer, Name: src.Name, Locked: src.Locked}
			}
		}
	}
	err := st.KeepRunLocalFolderWalls(ctx, run.ID, types.LocalFolderWallsRecord{Walls: walls, Layers: layers})
	if err == nil {
		return true
	}
	stopLease()
	s.failAndRevoke(ctx, run.ID, types.RunStarting, "the run's folder walls could not be stored")
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
		run.ID.String(), "failure", mustJSON(map[string]any{"error": auditStep(ctx, auditStepLocalFolderWalls, err, slog.String("run_id", run.ID.String()))})))
	return false
}

// runLocalFolderWalls is the run's stored walls; a store that keeps none is the zero record. A read error is
// returned, never "no walls".
func (s *Server) runLocalFolderWalls(ctx context.Context, runID uuid.UUID) (types.LocalFolderWallsRecord, error) {
	st, ok := s.cfg.Store.(store.RunLocalFolderWallsStore)
	if !ok {
		return types.LocalFolderWallsRecord{}, nil
	}
	return st.RunLocalFolderWalls(ctx, runID)
}

// runOnOwnComputer reports whether run is on the own runner of principal, the person opening its sync: placement
// local, a runner bound, and that runner person-owned by principal. A support helper or an admin who opens the sync
// of a run on someone else's computer is not on their own computer. A read error is returned.
func (s *Server) runOnOwnComputer(ctx context.Context, run types.AgentRun, principal string) (bool, error) {
	if run.Placement != types.PlacementLocal || run.RunnerID == nil {
		return false, nil
	}
	rs, ok := s.cfg.Store.(runnerGetter)
	if !ok {
		return false, nil
	}
	rn, err := rs.GetRunner(ctx, *run.RunnerID)
	if err != nil {
		return false, err
	}
	return rn.OwnerKind == types.RunnerOwnerPerson && rn.Owner == principal, nil
}

// localFolderByteCap passes at most left bytes of r; the next byte returns errLocalFolderCap and calls over once.
type localFolderByteCap struct {
	r    io.Reader
	left int64
	over func()
	once sync.Once
}

func (c *localFolderByteCap) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, errLocalFolderCap
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	if int64(n) > c.left {
		n, c.left = int(c.left), -1
		c.once.Do(c.over)
		return n, errLocalFolderCap
	}
	c.left -= int64(n)
	return n, err
}

// localFolderCapOf is the cap for way ("push" or "pull") in bytes and the wall that sets it; 0 is no cap.
func localFolderCapOf(walls *types.LocalFolderRules, way string) (int64, string) {
	switch {
	case walls == nil:
		return 0, ""
	case way == "pull":
		return int64(walls.MaxPullMiB) << 20, wallMaxPull
	}
	return int64(walls.MaxPushMiB) << 20, wallMaxPush
}

// syncCapEnd is the size-cap sentence a sync session was ended with, for its ssh.sync.transfer row's error; empty
// while the session is under its caps. The first cap reached wins.
type syncCapEnd struct {
	mu  sync.Mutex
	msg string
}

func (e *syncCapEnd) set(msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.msg == "" {
		e.msg = msg
	}
}

func (e *syncCapEnd) text() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.msg
}

// syncCapped bounds one way of a sync session by its cap: at the cap the session is audited, end keeps the size-cap
// sentence, and the session is ended with it and exit status syncCapExitStatus. r is returned as it is with no cap.
func (s *Server) syncCapped(runID uuid.UUID, principal string, channel ssh.Channel, rec types.LocalFolderWallsRecord, way string, end *syncCapEnd, r io.Reader) io.Reader {
	limit, wall := localFolderCapOf(rec.Walls, way)
	if r == nil || limit <= 0 {
		return r
	}
	l := rec.Layers[wall]
	return &localFolderByteCap{r: r, left: limit, over: func() {
		s.auditLocalFolderDenied(s.cfg.BaseCtx, runID, types.ActorHuman, principal, wall, l)
		msg := localFolderCapText(limit, way, l)
		end.set(msg)
		endSyncAtCap(channel, msg)
	}}
}

// syncCappedChannel is a sync channel whose client bytes are read through the push cap.
type syncCappedChannel struct {
	ssh.Channel
	r io.Reader
}

func (c syncCappedChannel) Read(p []byte) (int, error) { return c.r.Read(p) }

// syncPushCapped is channel with its client bytes read through the push cap, or channel itself with no cap.
func (s *Server) syncPushCapped(runID uuid.UUID, principal string, channel ssh.Channel, rec types.LocalFolderWallsRecord, end *syncCapEnd) ssh.Channel {
	if limit, _ := localFolderCapOf(rec.Walls, "push"); limit <= 0 {
		return channel
	}
	return syncCappedChannel{Channel: channel, r: s.syncCapped(runID, principal, channel, rec, "push", end, channel)}
}

// passCapped bounds one way of a computer-folder pass by its cap: at the cap the copy ends and the pass is audited.
func (s *Server) passCapped(runID uuid.UUID, rec types.LocalFolderWallsRecord, way string, r io.Reader) io.Reader {
	limit, wall := localFolderCapOf(rec.Walls, way)
	if r == nil || limit <= 0 {
		return r
	}
	l := rec.Layers[wall]
	return &localFolderByteCap{r: r, left: limit, over: func() {
		s.auditLocalFolderDenied(s.cfg.BaseCtx, runID, types.ActorSystem, "wardynd", wall, l)
	}}
}

// passStream is a computer-folder pass's stream to the runner (runnerwire.Stream).
type passStream interface {
	io.ReadWriter
	CloseWrite() error
}

// passPipes copies one computer-folder pass both ways, each through its cap: the runner's stream into the
// sandbox's sftp-server (push), and its replies back to the runner (pull). wait returns once both copies end.
func (s *Server) passPipes(runID uuid.UUID, rec types.LocalFolderWallsRecord, stream passStream, stdin io.WriteCloser, stdout io.Reader) (wait func()) {
	var joined sync.WaitGroup
	joined.Go(func() {
		_, _ = io.Copy(stdin, s.passCapped(runID, rec, "push", stream))
		_ = stdin.Close()
	})
	joined.Go(func() {
		_, _ = io.Copy(stream, s.passCapped(runID, rec, "pull", stdout))
		_ = stream.CloseWrite()
	})
	return joined.Wait
}

// syncWalls reads the run's walls for its sync channel and answers the client's sentence when they refuse it: an
// unreadable record, deny_remote for a run not on its person's own runner (audited), or an over-long deny union.
func (s *Server) syncWalls(ctx context.Context, run types.AgentRun, principal string) (types.LocalFolderWallsRecord, string) {
	rec, err := s.runLocalFolderWalls(ctx, run.ID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: a run's folder walls are unreadable; its sync is refused", "run_id", run.ID, "err", err)
		return rec, localFolderWallsUnreadableMsg
	}
	if rec.Walls == nil {
		return rec, ""
	}
	if rec.Walls.DenyRemote {
		own, err := s.runOnOwnComputer(ctx, run, principal)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: a run's runner is unreadable; its sync is refused", "run_id", run.ID, "err", err)
			return rec, localFolderWallsUnreadableMsg
		}
		if !own {
			l := rec.Layers[wallDenyRemote]
			s.auditLocalFolderDenied(s.cfg.BaseCtx, run.ID, types.ActorHuman, principal, wallDenyRemote, l)
			return rec, "wardyn: " + localFolderWalledText(wallDenyRemote, l)
		}
	}
	if _, err := servedSyncDeny(s.cfg.SyncDeny, rec.Walls.Deny); err != nil {
		return rec, "wardyn: " + err.Error()
	}
	return rec, ""
}

// syncCapExitStatus is the exit status a sync session ends with at a size cap: present, so wardyn sync --watch
// does not reconnect (it reconnects only when no status came), and not 1, so a cap reads apart from a refusal.
const syncCapExitStatus = 3

// endSyncAtCap ends a sync session at its size cap in sendChannelError's order: the sentence on stderr, the exit
// status, then close. sendChannelError keeps its status 1 for every refusal.
func endSyncAtCap(channel ssh.Channel, msg string) {
	_, _ = fmt.Fprintln(channel.Stderr(), msg)
	sendExitStatus(channel, syncCapExitStatus)
	_ = channel.Close()
}

// validateLocalFolders checks a policy's or boundary's local_folders: each cap 1 to 1048576 MiB when set, and the
// deny patterns one by one (checkLocalFolderDeny). The validators hold no deployment list, so the union with
// WARDYN_SYNC_DENY is checked again where the patterns are served.
func validateLocalFolders(lf *types.LocalFolderRules) error {
	if lf == nil {
		return nil
	}
	for _, c := range []struct {
		name string
		mib  int
	}{{wallMaxPush, lf.MaxPushMiB}, {wallMaxPull, lf.MaxPullMiB}} {
		if c.mib != 0 && (c.mib < 1 || c.mib > maxLocalFolderCapMiB) {
			return fmt.Errorf("local_folders.%s must be 1 to %d MiB; omit it for no limit", c.name, maxLocalFolderCapMiB)
		}
	}
	return checkLocalFolderDeny(nil, lf.Deny)
}

// checkLocalFolderDeny checks a run's deny patterns: each alone at most 256 bytes of UTF-8 with no control character
// (ParseSyncDeny's rules, never through a join, so a pattern may hold a comma), and the de-duplicated union with the
// deployment's at most 64.
func checkLocalFolderDeny(deploy, run []string) error {
	for _, p := range run {
		if len(p) > maxSyncDenyPattern || !utf8.ValidString(p) || strings.ContainsFunc(p, unicode.IsControl) {
			return fmt.Errorf("local_folders.deny: pattern %q must be at most %d bytes of UTF-8 with no control characters", p, maxSyncDenyPattern)
		}
	}
	if n := len(unionSyncDeny(deploy, run)); n > maxSyncDenyPatterns {
		return fmt.Errorf("local_folders.deny: %d patterns with the deployment's, at most %d", n, maxSyncDenyPatterns)
	}
	return nil
}

// unionSyncDeny is deploy then run, each pattern once, in that order.
func unionSyncDeny(deploy, run []string) []string {
	out := slices.Clone(deploy)
	for _, p := range run {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// servedSyncDeny is the deny list a sync is served: WARDYN_SYNC_DENY and the run's patterns, de-duplicated, or the
// checkLocalFolderDeny error when the union passes 64. Nothing is truncated.
func servedSyncDeny(deploy, run []string) ([]string, error) {
	if err := checkLocalFolderDeny(deploy, run); err != nil {
		return nil, err
	}
	return unionSyncDeny(deploy, run), nil
}

// folderPassArgs is a computer-folder pass's sftp-server argv: started in dir, and read-closed under deny_pull or a
// data_out rule other than allowed, as the sync channel is (bridgeSSHSync).
func (s *Server) folderPassArgs(ctx context.Context, runID uuid.UUID, dir string, walls *types.LocalFolderRules) []string {
	args := []string{"-d", dir}
	rule, _, err := s.runDataOutRule(ctx, runID) // an unreadable rule keeps content in, as the sync channel reads it
	if err != nil || rule.DataOut() != types.TransferAllowed || walls != nil && walls.DenyPull {
		args = append(args, "-P", sftpServerDeny)
	}
	return args
}

// walledSyncFolder starts a computer-folder pass with the run's deny patterns joined to args.Deny; a union over 64
// refuses the pass with errLocalFolderDenyOver.
func (s *Server) walledSyncFolder(ctx context.Context, sub *remote.Substrate, walls *types.LocalFolderRules, args runnerwire.SyncFolderArgs) (remote.FolderPass, error) {
	if walls != nil {
		deny, err := servedSyncDeny(args.Deny, walls.Deny)
		if err != nil {
			return remote.FolderPass{}, fmt.Errorf("%w: %w", errLocalFolderDenyOver, err)
		}
		args.Deny = deny
	}
	return sub.SyncFolder(ctx, args)
}

// runSyncViewFor is runSyncOpenView with the run's walls: the deny union, the caps in bytes, deny_pull with the
// CLI's sentence, and each wall's layer. A read error or an over-long union serves WARDYN_SYNC_DENY alone, and the
// channel then refuses.
func (s *Server) runSyncViewFor(ctx context.Context, runID uuid.UUID) runSyncView {
	v := s.runSyncOpenView(runID)
	rec, err := s.runLocalFolderWalls(ctx, runID)
	if err != nil || rec.Walls == nil {
		return v
	}
	deny, err := servedSyncDeny(s.cfg.SyncDeny, rec.Walls.Deny)
	if err != nil {
		return v
	}
	v.Deny = deny
	v.MaxPushBytes, _ = localFolderCapOf(rec.Walls, "push")
	v.MaxPullBytes, _ = localFolderCapOf(rec.Walls, "pull")
	if rec.Walls.DenyPull {
		v.DenyPull, v.DenyPullText = true, localFolderPullCLIText(rec)
	}
	v.WallLayers = rec.Layers
	return v
}

// refuseWalledBringBack answers the bring-back under deny_pull with 403 local_folder_walled (audited) and, when the
// walls cannot be read, 503 local_folder_walls_unreadable. It returns true on a refusal.
func (s *Server) refuseWalledBringBack(w http.ResponseWriter, r *http.Request, runID uuid.UUID, actorType types.ActorType, principal string) bool {
	rec, err := s.runLocalFolderWalls(r.Context(), runID)
	if err != nil {
		slog.WarnContext(r.Context(), "wardynd: a run's folder walls are unreadable; the bring-back is refused", "run_id", runID, "err", err)
		writeErrorReason(w, http.StatusServiceUnavailable, reasonLocalFolderWallsUnreadable, localFolderWallsUnreadableMsg)
		return true
	}
	if rec.Walls == nil || !rec.Walls.DenyPull {
		return false
	}
	s.auditLocalFolderDenied(r.Context(), runID, actorType, principal, wallDenyPull, rec.Layers[wallDenyPull])
	writeErrorReason(w, http.StatusForbidden, reasonLocalFolderWalled, localFolderBackText(rec))
	return true
}
