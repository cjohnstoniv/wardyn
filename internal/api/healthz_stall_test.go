package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// stalledHealthzStore stalls every read /healthz makes of the store: the sensor's beat (blockingHeartbeatStore),
// its node rows and the site settings. The site settings hold sign-in help, which must not be served late.
type stalledHealthzStore struct {
	blockingHeartbeatStore
}

func (stalledHealthzStore) UpsertGroundtruthSensor(context.Context, store.GroundtruthSensor) error {
	return nil
}

func (s stalledHealthzStore) ListGroundtruthSensors(ctx context.Context, _ time.Time) ([]store.GroundtruthSensor, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.released:
		return nil, nil
	}
}

func (s stalledHealthzStore) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	select {
	case <-ctx.Done():
		return types.SiteConfig{}, ctx.Err()
	case <-s.released:
		return types.SiteConfig{SignInHelpText: "Ask in #it-helpdesk.", SignInHelpURL: "https://it.corp.example/"}, nil
	}
}

// /healthz is the liveness probe: a store that stops answering must not hold it. Each read gives way at its
// bound, and the probe still answers 200 with the help left out and the sensor's verdict unreadable.
func TestHealthzAnswersAgainstAStalledStore(t *testing.T) {
	prevBeat, prevStore := sensorBeatReadTimeout, healthzStoreReadTimeout
	sensorBeatReadTimeout, healthzStoreReadTimeout = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { sensorBeatReadTimeout, healthzStoreReadTimeout = prevBeat, prevStore })
	released := make(chan struct{})
	defer close(released)
	h := newHarness(t)
	h.srv.cfg.Store = stalledHealthzStore{blockingHeartbeatStore{released: released}}

	type answer struct {
		rr   *httptest.ResponseRecorder
		took time.Duration
	}
	got := make(chan answer, 1)
	go func() {
		start := time.Now()
		rr := do(t, h.srv, http.MethodGet, "/healthz", "", "")
		got <- answer{rr, time.Since(start)}
	}()
	var a answer
	select {
	case a = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("GET /healthz did not answer against a stalled store: the probe would fail and restart the pod")
	}
	if a.took > 2*time.Second {
		t.Errorf("GET /healthz took %s against a stalled store, want each read cut off at its bound", a.took)
	}
	if a.rr.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d against a stalled store, want 200", a.rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(a.rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
	for _, k := range []string{"sign_in_help_text", "sign_in_help_url"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s served although the site settings read was cut off", k)
		}
	}
	gt, _ := body["ebpf_groundtruth"].(map[string]any)
	if gt["state"] != "unavailable" || gt["reason"] != groundtruthBeatReadFailed {
		t.Errorf("ebpf_groundtruth = %v, want state unavailable, reason %q", gt, groundtruthBeatReadFailed)
	}
}

// The bound stays inside the liveness probe's 3s with all three reads cut off in a row.
func TestHealthzStoreReadTimeoutStaysInsideTheProbes(t *testing.T) {
	if healthzStoreReadTimeout != 500*time.Millisecond {
		t.Fatalf("healthzStoreReadTimeout = %v, want 500ms", healthzStoreReadTimeout)
	}
}
