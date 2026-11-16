package critical_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// A closed poll stays public with its results, so no staff poll state, or the
// section that closes a poll, may say whether a poll is public; the two closed
// states keep distinct names.
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
	hidden := []string{"visib", "hidden", "hide", "public", "publish", "private", "sichtbar", "verborgen", "versteckt", "ausgeblendet", "öffentlich", "privat",
		"oculta", "oculto", "públic", "privad", "公開", "非表示", "공개", "숨", "可见", "隐藏", "公开", "发布"}
	for lang, words := range langs {
		t.Run(lang, func(t *testing.T) {
			list, editor := words.Manager, words.Editor
			if list["inactive"] == "" || list["inactive"] != editor["inactive"] {
				t.Errorf("poll list calls a switched-off poll %q, the editor %q", list["inactive"], editor["inactive"])
			}
			if list["closed"] == "" || strings.Contains(list["closed"], list["inactive"]) || strings.Contains(list["inactive"], list["closed"]) {
				t.Errorf("poll list names a poll past its close time %q, a switched-off one %q", list["closed"], list["inactive"])
			}
			for _, label := range []string{list["live"], list["scheduled"], list["closed"], list["inactive"], editor["inactive"], editor["schedule"]} {
				for _, word := range hidden {
					if strings.Contains(strings.ToLower(label), word) {
						t.Errorf("label %q says %q", label, word)
					}
				}
			}
		})
	}
}
