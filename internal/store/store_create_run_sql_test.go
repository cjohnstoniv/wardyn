// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestCreateRunBindsEveryInsertColumn: CreateRun's VALUES list is kept by
// hand beside runInsertCols, so two branches that each append a column and
// each bump "$N" to the same number merge cleanly into a statement one
// placeholder short — Postgres then refuses every run create. Train 17 merged
// exactly that (model_provider_id and user_type, both at $29).
func TestCreateRunBindsEveryInsertColumn(t *testing.T) {
	cols := strings.Count(runInsertCols, ",") + 1
	values := createRunSQL[strings.Index(createRunSQL, "VALUES"):strings.Index(createRunSQL, "RETURNING")]
	seen := map[string]bool{}
	for _, p := range regexp.MustCompile(`\$\d+`).FindAllString(values, -1) {
		seen[p] = true
	}
	for i := 1; i <= cols; i++ {
		if !seen["$"+strconv.Itoa(i)] {
			t.Errorf("CreateRun's VALUES list has no $%d for its %d runInsertCols columns", i, cols)
		}
	}
	if len(seen) != cols {
		t.Errorf("CreateRun binds %d placeholders for %d runInsertCols columns", len(seen), cols)
	}
}
