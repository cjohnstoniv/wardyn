// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"testing"
)

// macos_vm.max_base_age_days round-trips, and a document without the block decodes to nil and encodes without
// it, so a stored config from before it is unchanged.
func TestSiteConfigMacOSVMMaxBaseAgeDays(t *testing.T) {
	var sc SiteConfig
	if err := json.Unmarshal([]byte(`{"macos_vm":{"max_base_age_days":45}}`), &sc); err != nil || sc.MacOSVM == nil || sc.MacOSVM.MaxBaseAgeDays != 45 {
		t.Fatalf("decoded %+v, %v", sc.MacOSVM, err)
	}
	raw, err := json.Marshal(SiteConfig{})
	if err != nil || string(raw) != "{}" {
		t.Fatalf("an empty config encodes %s, %v", raw, err)
	}
}
