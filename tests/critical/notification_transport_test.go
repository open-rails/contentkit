package critical_test

import (
	"os/exec"
	"testing"
)

func TestShippedNotificationTransport(t *testing.T) {
	t.Run("CK-J25-S09_injected_transport_delegates_and_preserves_request_scope", func(t *testing.T) {
		command := exec.CommandContext(t.Context(), "node", "notification_transport.mjs")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("shipped notification transport: %v\n%s", err, output)
		}
		t.Log(string(output))
	})
}
