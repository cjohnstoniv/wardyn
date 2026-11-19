// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// orgPathLists holds the organisation's own lists; any other approval id is
// store.ErrNotFound, as push_content_paths answers it.
type orgPathLists struct {
	store.PushPathListStore
	lists map[uuid.UUID]types.PushPathList
	asked []uuid.UUID
}

func (l *orgPathLists) GetPushPathList(_ context.Context, id uuid.UUID) (types.PushPathList, error) {
	l.asked = append(l.asked, id)
	if pl, ok := l.lists[id]; ok {
		return pl, nil
	}
	return types.PushPathList{}, store.ErrNotFound
}

type exportStoreWithOrgLists struct {
	exportFaultStore
	*orgPathLists
}

// An enrolled device forwards its own held-push rows, whose path lists stay on
// the device. A mixed export carries the organisation's row with its paths and
// the device's row marked paths_unavailable held_on_device, and no lookup of
// the device's approval id can fail it.
func TestAuditExport_DevicePushPathRowIsMarkedNotLookedUp(t *testing.T) {
	orgID, devApproval, device := uuid.New(), uuid.New(), uuid.New()
	org := types.AuditEvent{ID: uuid.New(), Action: pushPathsAuditAction, Target: orgID.String(),
		ActorType: types.ActorAgent, Outcome: "success", Data: json.RawMessage(`{"paths_total":1}`)}
	dev := types.AuditEvent{ID: uuid.New(), Action: pushPathsAuditAction, Target: devApproval.String(),
		ActorType: types.ActorAgent, Actor: "device:" + device.String() + "/spiffe://local/run", Outcome: "success",
		Data:     json.RawMessage(`{"paths_total":2,"device_origin":{"device_id":"` + device.String() + `"}}`),
		DeviceID: &device}
	lists := &orgPathLists{lists: map[uuid.UUID]types.PushPathList{orgID: {Paths: []string{"a.go"}}}}
	st := &exportStoreWithOrgLists{exportFaultStore: exportFaultStore{pages: [][]types.AuditEvent{{org, dev}}, failAt: -1}, orgPathLists: lists}

	status, _, body, readErr := exportOverHTTP(t, st)
	if status != http.StatusOK || readErr != nil {
		t.Fatalf("status=%d err=%v body=%q, want 200 and a whole export", status, readErr, body)
	}
	rows := map[string]map[string]json.RawMessage{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		var ev types.AuditEvent
		var data map[string]json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil || json.Unmarshal(ev.Data, &data) != nil {
			t.Fatalf("export line %q: %v", sc.Bytes(), err)
		}
		rows[ev.Target] = data
	}
	if got := string(rows[orgID.String()]["paths"]); got != `["a.go"]` {
		t.Errorf("organisation row paths = %s, want its stored list", got)
	}
	d := rows[devApproval.String()]
	if got := string(d["paths_unavailable"]); got != `"held_on_device"` || d["paths"] != nil {
		t.Errorf("device row = %v, want paths_unavailable held_on_device and no paths", d)
	}
	for _, id := range lists.asked {
		if id == devApproval {
			t.Errorf("the device's approval id was looked up in the organisation's lists")
		}
	}
}

// A device's row is marked whatever its target says and whatever store serves
// the export: org ingest bounds a target's length only, so a device can forward
// a row whose target is not an approval id, and that row still says where its
// paths are.
func TestAuditExport_DevicePushPathRowIsMarkedWhateverItsTarget(t *testing.T) {
	device := uuid.New()
	dev := types.AuditEvent{ID: uuid.New(), Action: pushPathsAuditAction, Target: "not-an-approval-id",
		ActorType: types.ActorAgent, Actor: "device:" + device.String() + "/spiffe://local/run", Outcome: "success",
		Data:     json.RawMessage(`{"paths_total":2,"device_origin":{"device_id":"` + device.String() + `"}}`),
		DeviceID: &device}
	devUUID := dev
	devUUID.ID, devUUID.Target = uuid.New(), uuid.New().String()
	for _, c := range []struct {
		name string
		row  types.AuditEvent
		st   store.Store
	}{
		{"target not an approval id", dev, &exportStoreWithOrgLists{
			exportFaultStore: exportFaultStore{pages: [][]types.AuditEvent{{dev}}, failAt: -1}, orgPathLists: &orgPathLists{}}},
		{"no path-list store", devUUID, &exportFaultStore{pages: [][]types.AuditEvent{{devUUID}}, failAt: -1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, _, body, readErr := exportOverHTTP(t, c.st)
			if status != http.StatusOK || readErr != nil {
				t.Fatalf("status=%d err=%v body=%q, want 200 and a whole export", status, readErr, body)
			}
			var ev types.AuditEvent
			var data map[string]json.RawMessage
			if err := json.Unmarshal(bytes.TrimSpace(body), &ev); err != nil || json.Unmarshal(ev.Data, &data) != nil {
				t.Fatalf("export body %q: %v", body, err)
			}
			if got := string(data["paths_unavailable"]); got != `"held_on_device"` || data["paths"] != nil {
				t.Errorf("device row = %v, want paths_unavailable held_on_device and no paths", data)
			}
		})
	}
}
