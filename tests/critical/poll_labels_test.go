package critical_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// A closed poll stays public with its results, so no staff label for it, or
// for the section that closes it, may say it is hidden.
func TestPollStaffLabelsSayClosed(t *testing.T) {
	command := exec.CommandContext(t.Context(), "node", "--no-warnings", "poll_labels.mjs")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("poll label reader: %v", err)
	}
	var langs map[string]struct{ Manager, Editor map[string]string }
	must(t, json.Unmarshal(output, &langs))
	if len(langs) != 6 {
		t.Fatalf("read %d languages, want 6", len(langs))
	}
	hidden := []string{"visib", "hidden", "public", "sichtbar", "verborgen", "öffentlich", "oculta", "públic", "公開", "非表示", "공개", "숨김", "可见", "隐藏", "公开"}
	for lang, words := range langs {
		t.Run(lang, func(t *testing.T) {
			if words.Manager["inactive"] != words.Editor["inactive"] {
				t.Errorf("poll list calls a switched-off poll %q, the editor %q", words.Manager["inactive"], words.Editor["inactive"])
			}
			for _, key := range []string{"inactive", "schedule"} {
				for _, label := range []string{words.Manager[key], words.Editor[key]} {
					for _, word := range hidden {
						if strings.Contains(strings.ToLower(label), word) {
							t.Errorf("%s label %q says %q", key, label, word)
						}
					}
				}
			}
		})
	}
}
