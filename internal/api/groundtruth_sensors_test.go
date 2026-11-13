// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/recordmode"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// nodeSensorStore is a store with per-node sensor rows (store.GroundtruthSensorStore), one run, that
// run's audit events and the latest heartbeat, so the k8s ingest checks, /healthz and kernelWindow
// can be driven without Postgres.
type nodeSensorStore struct {
	store.Store
	mu      sync.Mutex
	sensors []store.GroundtruthSensor
	upserts []store.GroundtruthSensor
	run     types.AgentRun
	events  []types.AuditEvent
	beat    types.AuditEvent
	listErr error // ListGroundtruthSensors fails with it when set
}

func (s *nodeSensorStore) UpsertGroundtruthSensor(_ context.Context, g store.GroundtruthSensor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts = append(s.upserts, g)
	return nil
}

func (s *nodeSensorStore) ListGroundtruthSensors(_ context.Context, since time.Time) ([]store.GroundtruthSensor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []store.GroundtruthSensor
	for _, g := range s.sensors {
		if g.LastBeat.After(since) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *nodeSensorStore) LatestAuditEventByAction(context.Context, string) (types.AuditEvent, error) {
	if s.beat.Action == "" {
		return types.AuditEvent{}, store.ErrNotFound
	}
	return s.beat, nil
}

func (s *nodeSensorStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if id != s.run.ID {
		return types.AgentRun{}, store.ErrNotFound
	}
	return s.run, nil
}

func (s *nodeSensorStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return s.events, nil
}

func (*nodeSensorStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}

// nodeSensorServer builds a server over st with a fixed clock.
func nodeSensorServer(h *harness, st store.Store, now time.Time) *Server {
	cfg := baseTestConfig(h, st)
	cfg.Now = func() time.Time { return now }
	return New(cfg)
}

// podExec is a mapped kernel event for runID seen in pod ("namespace/name").
func podExec(runID uuid.UUID, pod, target string) types.AuditEvent {
	data, _ := json.Marshal(groundtruth.EventData{Stream: groundtruth.Stream, Subtype: "process_exec",
		Correlation: groundtruth.CorrelationMapped, Node: "node-a", Pod: pod})
	return types.AuditEvent{Action: groundtruth.ActionProcessExec, RunID: &runID, Target: target, Outcome: "success", Data: data}
}

// postBatch sends events as one ground-truth batch and wants 202.
func postBatch(t *testing.T, h *harness, srv *Server, events ...types.AuditEvent) {
	t.Helper()
	body, _ := json.Marshal(groundtruthBatch{Events: events})
	w := do(t, srv, http.MethodPost, "/api/v1/internal/groundtruth", h.mintGroundtruthToken(t), string(body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch code = %d, want 202 (a downgrade never rejects the batch); body=%s", w.Code, w.Body.String())
	}
}

// recordedByTarget returns the recorded event with target, or fails.
func recordedByTarget(t *testing.T, h *harness, target string) (types.AuditEvent, groundtruth.EventData) {
	t.Helper()
	for _, ev := range h.audit.snapshot() {
		if ev.Target == target {
			var ed groundtruth.EventData
			if err := json.Unmarshal(ev.Data, &ed); err != nil {
				t.Fatalf("decode %s data: %v", target, err)
			}
			return ev, ed
		}
	}
	t.Fatalf("no recorded event with target %s", target)
	return types.AuditEvent{}, groundtruth.EventData{}
}

// TestGroundtruthPodOutsideRunsNamespaceDowngrades: a k8s kernel event naming a pod in a namespace
// the Kubernetes driver does not place runs in loses its run (namespace_not_runs), while the same
// event from the runs namespace keeps it. Neither rejects the batch.
func TestGroundtruthPodOutsideRunsNamespaceDowngrades(t *testing.T) {
	t.Setenv("WARDYN_K8S_NAMESPACE", "wardyn-runs")
	h := newHarness(t)
	now := time.Now().UTC()
	runID := uuid.New()
	st := &nodeSensorStore{run: types.AgentRun{ID: runID, State: types.RunRunning, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}}
	srv := nodeSensorServer(h, st, now)

	name := "wardyn-agent-" + runID.String()
	postBatch(t, h, srv,
		podExec(runID, "other-team/"+name, "/bin/outside"),
		podExec(runID, "wardyn-runs/"+name, "/bin/inside"),
		podExec(runID, name, "/bin/no-namespace"))

	for _, target := range []string{"/bin/outside", "/bin/no-namespace"} {
		ev, ed := recordedByTarget(t, h, target)
		if ev.RunID != nil || ed.Correlation != groundtruth.CorrelationUnmapped || ed.Reason != "namespace_not_runs" {
			t.Errorf("%s: run=%v correlation=%q reason=%q, want run cleared, unmapped, namespace_not_runs", target, ev.RunID, ed.Correlation, ed.Reason)
		}
		if ed.Pod == "" || ed.Node != "node-a" {
			t.Errorf("%s: pod=%q node=%q, want both kept on the downgraded event", target, ed.Pod, ed.Node)
		}
	}
	if ev, ed := recordedByTarget(t, h, "/bin/inside"); ev.RunID == nil || *ev.RunID != runID || ed.Reason != "" {
		t.Errorf("runs-namespace event: run=%v reason=%q, want it kept on run %s", ev.RunID, ed.Reason, runID)
	}
}

// TestGroundtruthPodEventAfterRunEndedDowngrades: a k8s kernel event for a run that ended more than
// 15 minutes ago loses its run (run_ended). The clock is the terminal transition (EndedAt), not
// UpdatedAt, which later writes move. A run that ended 5 minutes ago keeps its late events, and a
// Compose event (no data.pod) for a long-ended run is unchanged.
func TestGroundtruthPodEventAfterRunEndedDowngrades(t *testing.T) {
	t.Setenv("WARDYN_K8S_NAMESPACE", "wardyn-runs")
	now := time.Now().UTC()
	pod := func(id uuid.UUID) string { return "wardyn-runs/wardyn-agent-" + id.String() }
	for name, tc := range map[string]struct {
		endedAgo   time.Duration
		pod        bool
		wantReason string
	}{
		"ended 16m ago, k8s event":     {endedAgo: 16 * time.Minute, pod: true, wantReason: "run_ended"},
		"ended 5m ago, k8s event":      {endedAgo: 5 * time.Minute, pod: true},
		"ended 16m ago, Compose event": {endedAgo: 16 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			runID := uuid.New()
			ended := now.Add(-tc.endedAgo)
			st := &nodeSensorStore{run: types.AgentRun{ID: runID, State: types.RunCompleted,
				CreatedAt: now.Add(-time.Hour), UpdatedAt: now, EndedAt: &ended}}
			srv := nodeSensorServer(h, st, now)
			ev := podExec(runID, pod(runID), "/bin/late")
			if !tc.pod {
				ev.Data = json.RawMessage(`{"stream":"ebpf","subtype":"process_exec","container_id":"abc","correlation":"mapped"}`)
			}
			postBatch(t, h, srv, ev)
			got, ed := recordedByTarget(t, h, "/bin/late")
			if tc.wantReason == "" {
				if got.RunID == nil || ed.Reason != "" {
					t.Fatalf("run=%v reason=%q, want the event kept on its run", got.RunID, ed.Reason)
				}
				return
			}
			if got.RunID != nil || ed.Reason != tc.wantReason || ed.Correlation != groundtruth.CorrelationUnmapped {
				t.Fatalf("run=%v correlation=%q reason=%q, want run cleared, unmapped, %s", got.RunID, ed.Correlation, ed.Reason, tc.wantReason)
			}
		})
	}
}

// TestGroundtruthExecMaskedWithItsRun: an exec's raw arguments carry a secret registered only for its
// run, one containing a space. The row is masked with that run's secrets before the arguments are
// split onto argv, both when the event keeps its run and when, for a run that ended over 15 minutes
// ago, it loses it. An event naming no run is split all the same and leaves no entry for the unknown
// id in the registry.
func TestGroundtruthExecMaskedWithItsRun(t *testing.T) {
	t.Setenv("WARDYN_K8S_NAMESPACE", "wardyn-runs")
	const secret = "alphaBravo charlieDelta"
	now := time.Now().UTC()
	for name, tc := range map[string]struct {
		endedAgo   time.Duration
		noRun      bool // the event names a run the store does not have
		wantReason string
		wantArgv   []string
	}{
		"kept on its run": {endedAgo: 5 * time.Minute, wantArgv: []string{"/usr/bin/mysql", "--password=<secret-hidden>", "--host", "db"}},
		"ended 16m ago":   {endedAgo: 16 * time.Minute, wantReason: "run_ended", wantArgv: []string{"/usr/bin/mysql", "--password=<secret-hidden>", "--host", "db"}},
		"names no run":    {noRun: true, wantReason: "run_id_not_found", wantArgv: []string{"/usr/bin/mysql", "--password=alphaBravo", "charlieDelta", "--host", "db"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			runID := uuid.New()
			ended := now.Add(-tc.endedAgo)
			cfg := baseTestConfig(h, &nodeSensorStore{run: types.AgentRun{ID: runID, State: types.RunCompleted,
				CreatedAt: now.Add(-time.Hour), UpdatedAt: now, EndedAt: &ended}})
			cfg.Now = func() time.Time { return now }
			cfg.MaskRegistry = secretmask.NewRegistry()
			if err := cfg.MaskRegistry.Add(runID, []byte(secret)); err != nil {
				t.Fatalf("register secret: %v", err)
			}
			named := runID
			if tc.noRun {
				named = uuid.New()
			}
			data, _ := json.Marshal(groundtruth.EventData{Stream: groundtruth.Stream, Subtype: groundtruth.SubtypeProcessExec,
				Correlation: groundtruth.CorrelationMapped, Node: "node-a", Pod: "wardyn-runs/wardyn-agent-" + named.String(),
				Argv: []string{"/usr/bin/mysql"}, Arguments: "--password=" + secret + " --host db"})
			postBatch(t, h, New(cfg), types.AuditEvent{Action: groundtruth.ActionProcessExec, RunID: &named,
				Target: "/usr/bin/mysql", Outcome: "success", Data: data})

			got, ed := recordedByTarget(t, h, "/usr/bin/mysql")
			if !slices.Equal(ed.Argv, tc.wantArgv) || ed.Arguments != "" {
				t.Errorf("argv = %q, arguments = %q, want %q and none", ed.Argv, ed.Arguments, tc.wantArgv)
			}
			if tc.noRun {
				if slices.Contains(cfg.MaskRegistry.RunIDs(), named) {
					t.Errorf("the registry holds an entry for %s, a run id the store does not have", named)
				}
			} else if strings.Contains(string(got.Data), "alphaBravo") || strings.Contains(string(got.Data), "charlieDelta") {
				t.Errorf("recorded data carries the run's secret: %s", got.Data)
			}
			if kept := tc.wantReason == ""; (got.RunID != nil) != kept || ed.Reason != tc.wantReason {
				t.Errorf("run=%v reason=%q, want kept=%v reason=%q", got.RunID, ed.Reason, kept, tc.wantReason)
			}
		})
	}
}

// TestGroundtruthExecSplitWaitsOnTheCorpus: the server that takes the batch has not read the run's secret yet, one
// containing a space that another replica registered. The raw arguments are split only after one read of the shared
// corpus for the batch. A read that brings the secret hides it whole; a read that fails stores the placeholder in
// place of the arguments, since words split under a short corpus can never be masked afterwards.
func TestGroundtruthExecSplitWaitsOnTheCorpus(t *testing.T) {
	t.Setenv("WARDYN_K8S_NAMESPACE", "wardyn-runs")
	const secret = "alphaBravo charlieDelta"
	now := time.Now().UTC()
	for name, tc := range map[string]struct {
		down      bool
		uncovered bool // the run's manifest cannot be proven here
		wantArgv  []string
	}{
		"the read brings the secret":          {wantArgv: []string{"/usr/bin/mysql", "--password=<secret-hidden>", "--host", "db"}},
		"the read fails":                      {down: true, wantArgv: []string{"/usr/bin/mysql", "<secret-hidden>"}},
		"the run's manifest cannot be proven": {uncovered: true, wantArgv: []string{"/usr/bin/mysql", "<secret-hidden>"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			runID := uuid.New()
			cfg := baseTestConfig(h, &nodeSensorStore{run: types.AgentRun{ID: runID, State: types.RunRunning,
				CreatedAt: now.Add(-time.Hour), UpdatedAt: now}})
			cfg.Now = func() time.Time { return now }
			reg, backend := newReadBackend(0)
			backend.stage(runID, secret)
			backend.down = tc.down
			cfg.MaskRegistry = reg
			if tc.uncovered {
				// A Postgres that refuses every connection (pgxpool dials lazily): Covered answers false.
				pool, err := pgxpool.New(context.Background(), "postgres://wardyn@127.0.0.1:1/wardyn?connect_timeout=1&sslmode=disable")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				cfg.MaskManifests = maskmanifest.New(pool, noKeys{}, reg)
			}
			data, _ := json.Marshal(groundtruth.EventData{Stream: groundtruth.Stream, Subtype: groundtruth.SubtypeProcessExec,
				Correlation: groundtruth.CorrelationMapped, Node: "node-a", Pod: "wardyn-runs/wardyn-agent-" + runID.String(),
				Argv: []string{"/usr/bin/mysql"}, Arguments: "--password=" + secret + " --host db"})
			exec := types.AuditEvent{Action: groundtruth.ActionProcessExec, RunID: &runID, Target: "/usr/bin/mysql",
				Outcome: "success", Data: data}
			postBatch(t, h, New(cfg), exec, exec)

			got, ed := recordedByTarget(t, h, "/usr/bin/mysql")
			if !slices.Equal(ed.Argv, tc.wantArgv) || ed.Arguments != "" {
				t.Errorf("argv = %q, arguments = %q, want %q and none", ed.Argv, ed.Arguments, tc.wantArgv)
			}
			if strings.Contains(string(got.Data), "alphaBravo") || strings.Contains(string(got.Data), "charlieDelta") {
				t.Errorf("recorded data carries the run's secret: %s", got.Data)
			}
			if n := backend.readCount(); n != 1 {
				t.Errorf("the shared corpus was read %d times for a batch of two execs, want once", n)
			}
		})
	}
}

// TestGroundtruthHeartbeatLeftAsSent: a process-wide masked value that also occurs in a beat, here its node name, does not
// rewrite the node its health row is written under. The handler leaves a heartbeat as the sensor sent it.
func TestGroundtruthHeartbeatLeftAsSent(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	st := &nodeSensorStore{}
	cfg := baseTestConfig(h, st)
	cfg.Now = func() time.Time { return now }
	cfg.MaskRegistry = secretmask.NewRegistry()
	if err := cfg.MaskRegistry.AddGlobal(0, "", "a-credential", now, []byte("node-alpha-0001")); err != nil {
		t.Fatalf("register a process-wide value: %v", err)
	}
	postBatch(t, h, New(cfg), groundtruth.HeartbeatEventForNode("node-alpha-0001", 3, 7, 2, map[string]uint64{groundtruth.ActionProcessExec: 7}))
	if len(st.upserts) != 1 || st.upserts[0].Node != "node-alpha-0001" || st.upserts[0].ObservedTotal != 7 {
		t.Fatalf("upserts = %+v, want one row for node-alpha-0001 with observed 7", st.upserts)
	}
}

// TestGroundtruthHeartbeatUpsertsNodeSensor: each kernel.sensor.ping writes its node's row with the
// server's clock and the beat's counts; the Compose sensor's row is the empty node.
func TestGroundtruthHeartbeatUpsertsNodeSensor(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	st := &nodeSensorStore{}
	srv := nodeSensorServer(h, st, now)
	k8s := groundtruth.HeartbeatEventForNode("node-a", 3, 7, 2, map[string]uint64{groundtruth.ActionProcessExec: 7})
	k8s.Time = now.Add(time.Hour) // a supplied time is ignored
	postBatch(t, h, srv, k8s, groundtruth.HeartbeatEventWithDropped(0, 1, 0, nil))

	if len(st.upserts) != 2 {
		t.Fatalf("upserts = %+v, want one per heartbeat", st.upserts)
	}
	a, compose := st.upserts[0], st.upserts[1]
	if a.Node != "node-a" || !a.LastBeat.Equal(now) || a.DroppedTotal != 3 || a.ObservedTotal != 7 || a.DroppedUnmapped != 2 ||
		a.ObservedByKind[groundtruth.ActionProcessExec] != 7 {
		t.Errorf("node-a row = %+v, want node-a at the server clock with 3/7/2 and process.exec=7", a)
	}
	if compose.Node != "" || compose.ObservedTotal != 1 {
		t.Errorf("Compose row = %+v, want the empty node with observed 1", compose)
	}
}

// TestGroundtruthHeartbeatNodeNameIsBounded: a node name past 253 bytes (a DNS name's limit) writes no
// sensor row, and the beat is still recorded and the batch still accepted.
func TestGroundtruthHeartbeatNodeNameIsBounded(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	st := &nodeSensorStore{}
	srv := nodeSensorServer(h, st, now)
	ok := strings.Repeat("a", 253)
	long := strings.Repeat("a", 254)
	postBatch(t, h, srv, groundtruth.HeartbeatEventForNode(ok, 0, 1, 0, nil), groundtruth.HeartbeatEventForNode(long, 0, 1, 0, nil))
	if len(st.upserts) != 1 || st.upserts[0].Node != ok {
		t.Fatalf("upserts = %+v, want only the 253-byte node", st.upserts)
	}
	beats := 0
	for _, ev := range h.audit.snapshot() {
		if ev.Action == groundtruth.ActionSensorHeartbeat {
			beats++
		}
	}
	if beats != 2 {
		t.Errorf("recorded heartbeats = %d, want both", beats)
	}
}

// healthyKinds is a breakdown that reads as healthy.
var healthyKinds = map[string]uint64{groundtruth.ActionProcessExec: 4, groundtruth.ActionNetworkConnect: 2}

// TestHealthzEbpfOneNodeMatchesToday pins the Compose and single-node output: with one live sensor
// row, ebpf_groundtruth is exactly what the latest heartbeat alone gives.
func TestHealthzEbpfOneNodeMatchesToday(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	beat := groundtruth.HeartbeatEventWithDropped(1, 6, 0, healthyKinds)
	beat.Time = now.Add(-30 * time.Second)

	today := nodeSensorServer(h, stubHeartbeatStore{ev: beat}, now)
	oneNode := nodeSensorServer(h, &nodeSensorStore{beat: beat, sensors: []store.GroundtruthSensor{
		{Node: "", LastBeat: beat.Time, DroppedTotal: 1, ObservedTotal: 6, ObservedByKind: healthyKinds},
		{Node: "gone-node", LastBeat: now.Add(-30 * time.Hour)},
	}}, now)

	want, _ := json.Marshal(today.ebpfGroundtruthStatus(context.Background()))
	got, _ := json.Marshal(oneNode.ebpfGroundtruthStatus(context.Background()))
	if string(got) != string(want) {
		t.Fatalf("one-node status = %s\nwant today's   %s", got, want)
	}
	golden := `{"dropped_total":1,"dropped_unmapped":0,"last_heartbeat":"` + beat.Time.Format(time.RFC3339) +
		`","observed_by_kind":{"kernel.network.connect":2,"kernel.process.exec":4},"observed_total":6,"state":"healthy"}`
	if string(got) != golden {
		t.Fatalf("one-node status = %s\nwant golden    %s", got, golden)
	}
}

// TestHealthzEbpfTwoNodesOneStaleDegraded: with two node sensors, one live node no longer masks a
// dead one. The state is the worst node's, a signed-in caller sees each node, and an anonymous one
// sees the verdict only. A node silent for over 24h is listed as gone and left out of the state.
func TestHealthzEbpfTwoNodesOneStaleDegraded(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	st := &nodeSensorStore{sensors: []store.GroundtruthSensor{
		{Node: "node-a", LastBeat: now.Add(-20 * time.Second), ObservedTotal: 6, ObservedByKind: healthyKinds},
		{Node: "node-b", LastBeat: now.Add(-5 * time.Minute), ObservedTotal: 3, ObservedByKind: healthyKinds},
		{Node: "node-c", LastBeat: now.Add(-25 * time.Hour), ObservedTotal: 9},
	}}
	srv := nodeSensorServer(h, st, now)
	h.srv = srv

	status := srv.ebpfGroundtruthStatus(context.Background())
	if status["state"] != "degraded" {
		t.Fatalf("state = %v, want degraded (node-b's beat is stale)", status["state"])
	}
	if status["observed_total"] != uint64(9) {
		t.Errorf("observed_total = %v, want 9 (the live nodes' sum; the gone node is left out)", status["observed_total"])
	}

	anon := healthzEbpf(t, h)
	if anon["state"] != "degraded" {
		t.Errorf("anonymous state = %v, want degraded", anon["state"])
	}
	if _, has := anon["sensors"]; has {
		t.Errorf("anonymous /healthz lists node sensors: %v", anon["sensors"])
	}

	w := do(t, srv, http.MethodGet, "/healthz", adminToken, "")
	var body struct {
		Ebpf struct {
			State   string `json:"state"`
			Sensors []struct {
				Node          string `json:"node"`
				State         string `json:"state"`
				LastHeartbeat string `json:"last_heartbeat"`
			} `json:"sensors"`
		} `json:"ebpf_groundtruth"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]string{}
	for _, s := range body.Ebpf.Sensors {
		got[s.Node] = s.State
		if s.LastHeartbeat == "" {
			t.Errorf("sensor %s has no last_heartbeat", s.Node)
		}
	}
	want := map[string]string{"node-a": "healthy", "node-b": "degraded", "node-c": "gone"}
	if len(got) != len(want) {
		t.Fatalf("sensors = %v, want %v", got, want)
	}
	for n, s := range want {
		if got[n] != s {
			t.Errorf("sensor %s state = %q, want %q (all: %v)", n, got[n], s, got)
		}
	}

	// The gone node is excluded: two healthy live nodes read healthy.
	st.sensors[1].LastBeat = now.Add(-10 * time.Second)
	if s := srv.ebpfGroundtruthStatus(context.Background())["state"]; s != "healthy" {
		t.Errorf("state with node-c gone and node-a, node-b fresh = %v, want healthy", s)
	}
}

// TestKernelWindowMultiNodeUsesRunNode: with several node sensors, the window reads the heartbeat of
// the node the run's own kernel events name, not the newest beat anywhere; with no kernel events it
// is empty.
func TestKernelWindowMultiNodeUsesRunNode(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	runID := uuid.New()
	run := types.AgentRun{ID: runID, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute)}
	st := &nodeSensorStore{run: run, sensors: []store.GroundtruthSensor{
		{Node: "node-a", LastBeat: now.Add(-2 * time.Minute), ObservedTotal: 50, DroppedUnmapped: 1},
		{Node: "node-b", LastBeat: now.Add(-3 * time.Minute), ObservedTotal: 0, DroppedUnmapped: 40},
	}}
	srv := nodeSensorServer(h, st, now)

	if got := srv.kernelWindow(context.Background(), run); got != (recordmode.KernelWindow{}) {
		t.Fatalf("no kernel events: window = %+v, want empty", got)
	}

	exec := podExec(runID, "wardyn-runs/wardyn-agent-"+runID.String(), "/bin/sh")
	exec.Data, _ = json.Marshal(groundtruth.EventData{Stream: groundtruth.Stream, Correlation: groundtruth.CorrelationMapped, Node: "node-b"})
	st.events = []types.AuditEvent{
		{Action: "egress.decision", RunID: &runID, Data: json.RawMessage(`{"node":"node-a"}`)},
		exec,
	}
	want := recordmode.KernelWindow{Beat: true, DroppedUnmapped: 40, ObservedTotal: 0}
	if got := srv.kernelWindow(context.Background(), run); got != want {
		t.Fatalf("window = %+v, want node-b's beat %+v", got, want)
	}

	// A node with no sensor row gives nothing rather than another node's beat.
	exec.Data, _ = json.Marshal(groundtruth.EventData{Stream: groundtruth.Stream, Correlation: groundtruth.CorrelationMapped, Node: "node-z"})
	st.events = []types.AuditEvent{exec}
	if got := srv.kernelWindow(context.Background(), run); got != (recordmode.KernelWindow{}) {
		t.Fatalf("unknown node: window = %+v, want empty", got)
	}
}

// runnerFactsRunner answers per-runner facts (runner.TargetInspector) from a fixed table; a runner
// not in it is unknown (offline, or nothing advertised yet).
type runnerFactsRunner struct {
	runner.Runner
	facts map[uuid.UUID]runner.TargetFacts
}

func (runnerFactsRunner) Name() string { return "remote" }

func (r runnerFactsRunner) RunnerFacts(id string) (runner.TargetFacts, bool) {
	f, ok := r.facts[uuid.MustParse(id)]
	return f, ok
}

func (runnerFactsRunner) ExecutorFacts(context.Context, string) (runner.TargetFacts, bool) {
	return runner.TargetFacts{}, false
}

// TestGroundtruthPodOnRunnerHeldToRunnerNamespaces: a run on a self-hosted runner lives in that
// runner's cluster, so its events are held to the namespaces the runner advertises, not to the
// control plane's own. Where the control plane has no source for the run's namespaces (the runner
// is unknown or advertises none, or the run is on a remote-hosted runner) the check is skipped and
// the event keeps its run.
func TestGroundtruthPodOnRunnerHeldToRunnerNamespaces(t *testing.T) {
	t.Setenv("WARDYN_K8S_NAMESPACE", "wardyn-runs")
	now := time.Now().UTC()
	known, bare, offline := uuid.New(), uuid.New(), uuid.New()
	rn := runnerFactsRunner{facts: map[uuid.UUID]runner.TargetFacts{
		known: {RunsNamespaces: []string{"team-runs", "team-runs-2"}},
		bare:  {},
	}}
	for name, tc := range map[string]struct {
		runnerID   *uuid.UUID
		executor   string
		ns         string
		wantReason string
	}{
		"runner namespace":                     {runnerID: &known, ns: "team-runs-2"},
		"outside the runner's namespaces":      {runnerID: &known, ns: "other-team", wantReason: "namespace_not_runs"},
		"control plane namespace on a runner":  {runnerID: &known, ns: "wardyn-runs", wantReason: "namespace_not_runs"},
		"runner advertises no namespaces":      {runnerID: &bare, ns: "anything"},
		"runner unknown to the control plane":  {runnerID: &offline, ns: "anything"},
		"remote-hosted runner":                 {executor: "hosted-runner-1", ns: "anything"},
		"in-process driver, other namespace":   {ns: "team-runs", wantReason: "namespace_not_runs"},
		"in-process driver, its own namespace": {ns: "wardyn-runs"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			runID := uuid.New()
			st := &nodeSensorStore{run: types.AgentRun{ID: runID, State: types.RunRunning, CreatedAt: now.Add(-time.Hour),
				UpdatedAt: now, RunnerID: tc.runnerID, RunnerPoolExecutor: tc.executor}}
			cfg := baseTestConfig(h, st)
			cfg.Now = func() time.Time { return now }
			cfg.Runner = rn
			srv := New(cfg)
			postBatch(t, h, srv, podExec(runID, tc.ns+"/wardyn-agent-"+runID.String(), "/bin/ev"))
			got, ed := recordedByTarget(t, h, "/bin/ev")
			if tc.wantReason == "" {
				if got.RunID == nil || ed.Reason != "" {
					t.Fatalf("run=%v reason=%q, want the event kept on its run", got.RunID, ed.Reason)
				}
				return
			}
			if got.RunID != nil || ed.Reason != tc.wantReason {
				t.Fatalf("run=%v reason=%q, want run cleared with %s", got.RunID, ed.Reason, tc.wantReason)
			}
		})
	}
}

// TestGroundtruthPodEventAfterLeaseEndedDowngrades: a run whose lease ended stays RUNNING until
// the ended-run grace, but its pod is gone from the time the lease ended (LostAt), so a k8s event
// more than 15 minutes after that loses its run. Another lost reason keeps the run.
func TestGroundtruthPodEventAfterLeaseEndedDowngrades(t *testing.T) {
	t.Setenv("WARDYN_K8S_NAMESPACE", "wardyn-runs")
	now := time.Now().UTC()
	for name, tc := range map[string]struct {
		reason     types.LostReason
		lostAgo    time.Duration
		wantReason string
	}{
		"lease ended 16m ago": {reason: types.LostEnded, lostAgo: 16 * time.Minute, wantReason: "run_ended"},
		"lease ended 5m ago":  {reason: types.LostEnded, lostAgo: 5 * time.Minute},
		"rebooted 16m ago":    {reason: types.LostReboot, lostAgo: 16 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			runID := uuid.New()
			lost := now.Add(-tc.lostAgo)
			st := &nodeSensorStore{run: types.AgentRun{ID: runID, State: types.RunRunning, CreatedAt: now.Add(-time.Hour),
				UpdatedAt: now, LostAt: &lost, LostReason: tc.reason}}
			srv := nodeSensorServer(h, st, now)
			postBatch(t, h, srv, podExec(runID, "wardyn-runs/wardyn-agent-"+runID.String(), "/bin/late"))
			got, ed := recordedByTarget(t, h, "/bin/late")
			if tc.wantReason == "" {
				if got.RunID == nil || ed.Reason != "" {
					t.Fatalf("run=%v reason=%q, want the event kept on its run", got.RunID, ed.Reason)
				}
				return
			}
			if got.RunID != nil || ed.Reason != tc.wantReason {
				t.Fatalf("run=%v reason=%q, want run cleared with %s", got.RunID, ed.Reason, tc.wantReason)
			}
		})
	}
}

// TestHealthzEbpfNodeReadFailureIsDegraded: a failed per-node read (say a split-role install missing
// the grant on groundtruth_sensors) must not fall back to the newest heartbeat anywhere, which would
// read healthy while another node is dead. It reads degraded, with the same reason anonymously. A
// successful read of zero nodes keeps today's answer.
func TestHealthzEbpfNodeReadFailureIsDegraded(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	beat := groundtruth.HeartbeatEventWithDropped(0, 6, 0, healthyKinds)
	beat.Time = now.Add(-10 * time.Second) // fresh and healthy on its own

	srv := nodeSensorServer(h, &nodeSensorStore{beat: beat, listErr: errors.New("permission denied for table groundtruth_sensors")}, now)
	h.srv = srv
	status := srv.ebpfGroundtruthStatus(context.Background())
	if status["state"] != "degraded" || status["reason"] != "Node sensor health could not be read" {
		t.Fatalf("status = %v, want degraded with the node-read reason", status)
	}
	if anon := healthzEbpf(t, h); anon["state"] != "degraded" || anon["reason"] != "Node sensor health could not be read" {
		t.Errorf("anonymous ebpf_groundtruth = %v, want degraded with the same reason", anon)
	}

	today := nodeSensorServer(h, stubHeartbeatStore{ev: beat}, now)
	zero := nodeSensorServer(h, &nodeSensorStore{beat: beat}, now)
	want, _ := json.Marshal(today.ebpfGroundtruthStatus(context.Background()))
	got, _ := json.Marshal(zero.ebpfGroundtruthStatus(context.Background()))
	if string(got) != string(want) {
		t.Fatalf("zero-node status = %s\nwant today's    %s", got, want)
	}
}

// pagedNodeSensorStore is nodeSensorStore on a paging store: kernelWindowOnNode must find the run's node
// through the filtered, newest-first, bounded read and never the whole-trail QueryAuditEvents.
type pagedNodeSensorStore struct {
	*nodeSensorStore
	store.Pager
	kernel   types.AuditEvent
	filter   store.AuditFilter
	page     store.Page
	pagedFor *uuid.UUID
	fullRead bool
}

func (s *pagedNodeSensorStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	s.fullRead = true
	return nil, nil
}

func (s *pagedNodeSensorStore) QueryAuditEventsFilteredPage(_ context.Context, runID *uuid.UUID, f store.AuditFilter, p store.Page) ([]types.AuditEvent, error) {
	s.pagedFor, s.filter, s.page = runID, f, p
	return []types.AuditEvent{s.kernel}, nil
}

func TestKernelWindowOnNodeReadsABoundedKernelWindow(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	runID := uuid.New()
	run := types.AgentRun{ID: runID, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute)}
	exec := podExec(runID, "wardyn-runs/wardyn-agent-"+runID.String(), "/bin/sh")
	exec.Data, _ = json.Marshal(groundtruth.EventData{Stream: groundtruth.Stream, Correlation: groundtruth.CorrelationMapped, Node: "node-b"})
	st := &pagedNodeSensorStore{kernel: exec, nodeSensorStore: &nodeSensorStore{run: run, sensors: []store.GroundtruthSensor{
		{Node: "node-a", LastBeat: now.Add(-2 * time.Minute), ObservedTotal: 50, DroppedUnmapped: 1},
		{Node: "node-b", LastBeat: now.Add(-3 * time.Minute), ObservedTotal: 0, DroppedUnmapped: 40},
	}}}
	srv := nodeSensorServer(h, st, now)

	want := recordmode.KernelWindow{Beat: true, DroppedUnmapped: 40, ObservedTotal: 0}
	if got := srv.kernelWindow(context.Background(), run); got != want {
		t.Fatalf("window = %+v, want node-b's beat %+v", got, want)
	}
	if st.fullRead {
		t.Fatal("kernelWindowOnNode read the run's whole audit trail")
	}
	if st.pagedFor == nil || *st.pagedFor != runID || st.filter.ActionPrefix != groundtruth.KernelActionPrefix ||
		!st.page.Newest || st.page.Limit != kernelNodeEventWindow {
		t.Fatalf("paged read = run %v filter %+v page %+v, want this run's newest %d kernel events", st.pagedFor, st.filter, st.page, kernelNodeEventWindow)
	}
}
