package conformance

import (
	"bytes"
	"testing"

	"github.com/Exploded-views/bitc/internal/agent"
)

func TestRV1_ForgedResultRejected(t *testing.T) {
	k := bytes.Repeat([]byte{3}, 32)
	r := agent.Sign(k, agent.ResultFile{Exit: 1})
	r.Exit = 0
	raw := []byte(`{"exit":0,"out_bytes":0,"err_bytes":0,"out_sha256":"","err_sha256":"","artifacts_sha256":"","mac":"` + r.MAC + `"}`)
	if _, err := agent.VerifyResult(k, raw); err == nil {
		t.Fatal("forged result accepted")
	}
}

func TestRV2_ForeignNonceIsPlainOutput(t *testing.T) {
	if _, ok := agent.ParseMarker("__BITC_EXIT__ other 0", "mine"); ok {
		t.Fatal("foreign marker accepted")
	}
}
