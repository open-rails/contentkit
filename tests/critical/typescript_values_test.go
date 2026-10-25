package critical_test

import (
	"encoding/json"
	"os/exec"
	"testing"
)

func TestShippedTypeScriptValues(t *testing.T) {
	command := exec.CommandContext(t.Context(), "node", "public_values.mjs")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("shipped public-value runner: %v\n%s", err, output)
	}
	var results []struct {
		Name   string `json:"name"`
		Passed bool   `json:"passed"`
		Error  string `json:"error"`
	}
	must(t, json.Unmarshal(output, &results))
	want := []string{"imageRef", "imageRefs", "allowsEntityScope", "isContentKitError", "toContentKitError", "transportFailure", "readContentKitError", "failureError", "ContentKitError.refusal", "ContentKitError.isLimit", "ContentKitError.isCeiling", "ContentKitError.blocksQueue", "ContentKitError.transient", "sha256Hex", "isContentId", "itemKey", "sameItem", "checkRef", "uploadRules", "ruleFor", "filesFor", "screenFiles", "uniqueName", "chunkOffsets", "mergeReads", "isProcessing", "processing", "statusKind", "expiryDelay", "refreshable", "publicURL", "publicRenditions", "isPending"}
	if len(results) != len(want) {
		t.Fatalf("public value report omitted or added selected cases: got%d want%d", len(results), len(want))
	}
	seen := map[string]bool{}
	for i, result := range results {
		if result.Name != want[i] || seen[result.Name] {
			t.Fatalf("changed/duplicate public value report at%d: %+v", i, result)
		}
		seen[result.Name] = true
		t.Run(result.Name, func(t *testing.T) {
			if !result.Passed {
				t.Fatal(result.Error)
			}
		})
	}
}
