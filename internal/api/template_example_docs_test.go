// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The Azure DevOps example template is documented in docs/AZURE-DEVOPS.md. It
// must pass the same door an import goes through, and the docs must show the
// file as it is.
func TestADOContributeTemplateExampleImports(t *testing.T) {
	src, err := os.ReadFile("../../examples/templates/ado-contribute-partial.yaml")
	if err != nil {
		t.Fatal(err)
	}
	res := validateTemplateImport(src, client.TemplateFormatYAML, "RunTemplate")
	if len(res.Diagnostics) != 0 || res.Document == nil {
		t.Fatalf("example refused: %+v", res.Diagnostics)
	}
	if res.Document.Coverage != client.TemplateCoveragePartial {
		t.Errorf("coverage = %q, want partial", res.Document.Coverage)
	}
	doc, err := os.ReadFile("../../docs/AZURE-DEVOPS.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "```yaml\n"+string(src)+"```") {
		t.Error("docs/AZURE-DEVOPS.md does not quote examples/templates/ado-contribute-partial.yaml verbatim")
	}
}
