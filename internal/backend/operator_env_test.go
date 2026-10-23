package backend

import (
	"strings"
	"testing"
)

func TestOperatorValuesRetainDiagnosticRoutingWithInvalidPendingRuntimeEdit(t *testing.T) {
	raw := []byte("MC_ADMIN_SOCKET=/run/mcs-bootloader/control.sock\nMC_IDLE_TIMEOUT=garbage\nMC_MOTD='unfinished\nMC_RCON_PASSWORD='private-literal $(not expanded)'\nMC_LOG_NAMESPACE=private\n")
	if _, err := LoadEnv(raw); err == nil {
		t.Fatal("fixture should fail runtime validation")
	}
	values, err := OperatorValues(raw)
	if err != nil {
		t.Fatal(err)
	}
	if values["MC_ADMIN_SOCKET"] != "/run/mcs-bootloader/control.sock" || values["MC_LOG_NAMESPACE"] != "private" || values["MC_RCON_PASSWORD"] != "private-literal $(not expanded)" {
		t.Fatalf("routing literals changed: %v", len(values))
	}
	if _, exists := values["MC_MOTD"]; exists {
		t.Fatal("unrelated pending data escaped routing parser")
	}
}
func TestOperatorValuesRejectAmbiguousRoutingWithoutLeakingValues(t *testing.T) {
	for _, text := range []string{"MC_ADMIN_SOCKET=/run/first\nMC_ADMIN_SOCKET=/run/second\n", "MC_RCON_PASSWORD='private-secret-unclosed", "MC_LOG_NAMESPACE=bad\x00text"} {
		_, err := OperatorValues([]byte(text))
		if err == nil {
			t.Fatal("malformed routing accepted")
		}
		if strings.Contains(err.Error(), "private-secret") {
			t.Fatal("secret leaked")
		}
	}
}
