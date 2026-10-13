package federation

import (
	"strings"
	"testing"
)

// A plain-http organisation URL is refused in the runner's own words, not with
// advice about the control plane's WARDYN_CONTROL_PLANE_URL.
func TestCheckOrgURLRefusalIsTheRunnerSentence(t *testing.T) {
	for _, raw := range []string{"http://example.com", "ftp://localhost"} {
		err := CheckOrgURL(raw)
		if err == nil {
			t.Fatalf("accepted %q", raw)
		}
		if strings.Contains(err.Error(), "WARDYN_CONTROL_PLANE_URL") {
			t.Errorf("%q: %v mentions WARDYN_CONTROL_PLANE_URL", raw, err)
		}
		if !strings.Contains(err.Error(), "runner organisation URL must be https://") {
			t.Errorf("%q: %v is not the runner sentence", raw, err)
		}
	}
}
