// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A door's detail can neither set nor clear the marker: the context decides,
// by overwrite when marked and by deletion when not.
func TestDryRunRecorderOwnsTheMarker(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx  context.Context
		data string
		want string
	}{
		"marked, no data":           {WithDryRun(context.Background()), ``, `{"dry_run":true}`},
		"marked, detail clears it":  {WithDryRun(context.Background()), `{"reason":"r","dry_run":false}`, `{"dry_run":true,"reason":"r"}`},
		"marked, non-object data":   {WithDryRun(context.Background()), `[1]`, `{"data":[1],"dry_run":true}`},
		"unmarked, detail sets it":  {context.Background(), `{"reason":"r","dry_run":true}`, `{"reason":"r"}`},
		"unmarked, untouched":       {context.Background(), `{"reason":"r"}`, `{"reason":"r"}`},
		"unmarked, non-object data": {context.Background(), `"dry_run"`, `"dry_run"`},
	} {
		t.Run(name, func(t *testing.T) {
			inner := &captureRecorder{}
			ev := types.AuditEvent{Action: "x.y.z", Data: json.RawMessage(tc.data)}
			if err := (DryRunRecorder{Inner: inner}).Record(tc.ctx, ev); err != nil {
				t.Fatal(err)
			}
			if got := string(inner.got[0].Data); got != tc.want {
				t.Errorf("data = %s, want %s", got, tc.want)
			}
		})
	}
}
