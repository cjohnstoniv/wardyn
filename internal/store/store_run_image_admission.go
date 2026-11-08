// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SetRunImageAdmission scoped-writes ONLY the run's image admission verdict (nil clears it): a launch's check
// of the convention image under scope all, made after the row exists.
func (s PG) SetRunImageAdmission(ctx context.Context, id uuid.UUID, v *types.ImageAdmissionVerdict) error {
	raw, err := imageAdmissionJSON(v)
	if err != nil {
		return err
	}
	return s.execRun(ctx, "set run image admission",
		`UPDATE agent_runs SET image_admission=$1, updated_at=now() WHERE id=$2`, raw, id)
}

// imageAdmissionJSON is the image_admission column's value: NULL for a run with no verdict.
func imageAdmissionJSON(v *types.ImageAdmissionVerdict) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("store: marshal run image admission: %w", err)
	}
	return raw, nil
}

// scanRunImageAdmission reads the image_admission column back: nil for NULL. A verdict that does not decode
// is an error, never no verdict, so a run is never read as unchecked when it was checked.
func scanRunImageAdmission(raw []byte) (*types.ImageAdmissionVerdict, error) {
	if raw == nil {
		return nil, nil
	}
	var v types.ImageAdmissionVerdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("store: unmarshal run image admission: %w", err)
	}
	return &v, nil
}
