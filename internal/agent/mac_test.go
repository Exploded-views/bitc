package agent

import (
	"encoding/json"
	"testing"
)

func TestResultMAC(t *testing.T) {
	k := []byte("0123456789abcdef0123456789abcdef")
	r := Sign(k, ResultFile{Exit: 3, OutBytes: 10, ErrBytes: 0, OutSHA256: "a", ErrSHA256: "b", ArtifactsSHA256: "c"})
	raw, _ := json.Marshal(r)
	if got, err := VerifyResult(k, raw); err != nil || got.Exit != 3 {
		t.Fatal(got, err)
	}
	forged := r
	forged.Exit = 0
	raw, _ = json.Marshal(forged)
	if _, err := VerifyResult(k, raw); err == nil {
		t.Fatal("forged exit accepted")
	}
	raw, _ = json.Marshal(r)
	if _, err := VerifyResult([]byte("another key another key another k"), raw); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestStateMACBoundAndDistinct(t *testing.T) {
	k := []byte("0123456789abcdef0123456789abcdef")
	exit := 7
	s := SignState(k, State{Phase: "ended", Exit: &exit, Job: "j1", Nonce: "aa", Seq: 3, TS: 5})
	raw, _ := json.Marshal(s)
	if got, err := VerifyState(k, raw, "j1", "aa"); err != nil || got.Seq != 3 || *got.Exit != 7 {
		t.Fatal(got, err)
	}
	for name, c := range map[string]struct {
		k          []byte
		job, nonce string
	}{
		"wrong key":   {[]byte("another key another key another k"), "j1", "aa"},
		"other job":   {k, "j2", "aa"},
		"other nonce": {k, "j1", "bb"},
	} {
		if _, err := VerifyState(c.k, raw, c.job, c.nonce); err == nil {
			t.Fatal(name + " accepted")
		}
	}
	forged := s
	forged.Phase = "begun"
	raw, _ = json.Marshal(forged)
	if _, err := VerifyState(k, raw, "j1", "aa"); err == nil {
		t.Fatal("forged phase accepted")
	}
	if _, err := VerifyState(k, []byte(`{"phase":"begun"}`), "j1", "aa"); err == nil {
		t.Fatal("unsigned state accepted")
	}
	// a state MAC is never a result MAC
	r := Sign(k, ResultFile{Exit: 7})
	raw, _ = json.Marshal(map[string]any{"phase": "ended", "exit": 7, "job": "j1", "nonce": "aa", "mac": r.MAC})
	if _, err := VerifyState(k, raw, "j1", "aa"); err == nil {
		t.Fatal("result MAC accepted as state MAC")
	}
}
