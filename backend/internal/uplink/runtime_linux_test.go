package uplink

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimePolicyFailsClosed(t *testing.T) {
	old := runtimePolicyPath
	runtimePolicyPath = filepath.Join(t.TempDir(), "egress.json")
	defer func() { runtimePolicyPath = old }()
	if DefaultNetwork() != "" {
		t.Fatal("missing policy changed legacy behavior")
	}
	if err := os.WriteFile(runtimePolicyPath, []byte(`{"primary":"bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if DefaultNetwork() != "unavailable" {
		t.Fatal("invalid policy fell back")
	}
	n := NetworkInfo{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if _, err := runtimeUplink(n); err == nil {
		t.Fatal("invalid policy allowed direct")
	}
	data := `{"primary":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","networks":{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":{"enabled":true,"ready":false,"epoch":"1"}}}`
	if err := os.WriteFile(runtimePolicyPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeUplink(n); err == nil {
		t.Fatal("failed VPN allowed direct")
	}
	if runtimeSignature(n.ID) != "blocked:1" {
		t.Fatal("incorrect blocked status")
	}
}
