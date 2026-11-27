// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"strings"
	"testing"
)

func TestDiskInfoRefusesBackingAndDataFile(t *testing.T) {
	const standalone = `{"virtual-size": 68719476736, "filename": "src.qcow2", "format": "qcow2",
		"format-specific": {"type": "qcow2", "data": {"compat": "1.1", "lazy-refcounts": false}}}`
	di, err := ParseDiskInfo([]byte(standalone))
	if err != nil || di.VirtualSize != 64<<30 {
		t.Fatalf("a standalone qcow2 = %+v, %v", di, err)
	}
	for name, tc := range map[string]struct{ json, want string }{
		"backing file": {`{"virtual-size": 1073741824, "format": "qcow2", "backing-filename": "/etc/shadow",
			"full-backing-filename": "/etc/shadow", "format-specific": {"type": "qcow2", "data": {}}}`, "backing file"},
		"full backing only": {`{"virtual-size": 1073741824, "format": "qcow2", "full-backing-filename": "/x"}`, "backing file"},
		"data file": {`{"virtual-size": 1073741824, "format": "qcow2",
			"format-specific": {"type": "qcow2", "data": {"data-file": "/dev/sda", "data-file-raw": true}}}`, "external data file"},
		"raw":     {`{"virtual-size": 1073741824, "format": "raw"}`, "not qcow2"},
		"no size": {`{"format": "qcow2"}`, "no size"},
		"garbage": {`not json`, "qemu-img info"},
	} {
		if _, err := ParseDiskInfo([]byte(tc.json)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want a refusal naming %q", name, err, tc.want)
		}
	}
}
