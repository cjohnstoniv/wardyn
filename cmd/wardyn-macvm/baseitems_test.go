// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// --base-file loads the install list into stage 2; a list that does not parse stops the job before any
// boot.
func TestStage2LoadsTheInstallList(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "items.json")
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(good, []byte(`{"version":1,"items":[{"kind":"pkg","name":"edr","path":"/opt/edr.pkg","sha256":"`+strings.Repeat("a", 64)+`"}]}`), 0o600)
	_ = os.WriteFile(bad, []byte(`{"version":1,"items":[{"kind":"script","name":"x","path":"/x","sha256":"`+strings.Repeat("a", 64)+`"}]}`), 0o600)
	for path, want := range map[string]int{good: 1, "": 0, bad: -1} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		j := newJobFlags(fs)
		if err := fs.Parse([]string{"--" + macvm.FlagBaseFile, path}); err != nil {
			t.Fatal(err)
		}
		st, err := j.stage2(nil, deps{}, nil, "/agent")
		switch {
		case want < 0 && err == nil:
			t.Errorf("%s: accepted", path)
		case want >= 0 && (err != nil || len(st.Items.Items) != want):
			t.Errorf("%s: %v %+v", path, err, st)
		}
	}
}
