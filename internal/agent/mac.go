package agent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// ResultFile is result.json in the pod (spec 8.4 step 6).
type ResultFile struct {
	Exit            int    `json:"exit"`
	OutBytes        int64  `json:"out_bytes"`
	ErrBytes        int64  `json:"err_bytes"`
	OutSHA256       string `json:"out_sha256"`
	ErrSHA256       string `json:"err_sha256"`
	ArtifactsSHA256 string `json:"artifacts_sha256"`
	MAC             string `json:"mac"`
}

// canonical is the JSON of every field but mac, keys sorted, no spaces.
func canonical(r ResultFile) []byte {
	b, _ := json.Marshal(map[string]any{"exit": r.Exit, "out_bytes": r.OutBytes, "err_bytes": r.ErrBytes,
		"out_sha256": r.OutSHA256, "err_sha256": r.ErrSHA256, "artifacts_sha256": r.ArtifactsSHA256})
	return b
}

func Sign(k []byte, r ResultFile) ResultFile {
	m := hmac.New(sha256.New, k)
	m.Write(canonical(r))
	r.MAC = hex.EncodeToString(m.Sum(nil))
	return r
}

// VerifyResult accepts result.json only if its MAC verifies under K.
func VerifyResult(k []byte, raw []byte) (ResultFile, error) {
	var r ResultFile
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	want := Sign(k, r).MAC
	if !hmac.Equal([]byte(want), []byte(r.MAC)) {
		return r, errors.New("result failed verification")
	}
	return r, nil
}

// stateDomain separates a state MAC from a result MAC: the state MAC input starts with it, the
// result MAC input (a JSON object) never does.
const stateDomain = "bitc-state-v1\n"

// State is state.json: the phase, bound to one job and one attempt (nonce M), ordered by Seq
// and MACed with K like result.json (spec 8.4).
type State struct {
	Phase string `json:"phase"`
	Exit  *int   `json:"exit,omitempty"`
	Job   string `json:"job"`
	Nonce string `json:"nonce"`
	Seq   int64  `json:"seq"`
	TS    int64  `json:"ts"`
	MAC   string `json:"mac"`
}

func stateCanonical(s State) []byte {
	b, _ := json.Marshal(map[string]any{"phase": s.Phase, "exit": s.Exit, "job": s.Job,
		"nonce": s.Nonce, "seq": s.Seq, "ts": s.TS})
	return append([]byte(stateDomain), b...)
}

func SignState(k []byte, s State) State {
	m := hmac.New(sha256.New, k)
	m.Write(stateCanonical(s))
	s.MAC = hex.EncodeToString(m.Sum(nil))
	return s
}

// VerifyState accepts state.json only if its MAC verifies under K and it names this job and
// this attempt's nonce.
func VerifyState(k []byte, raw []byte, job, nonce string) (State, error) {
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	if !hmac.Equal([]byte(SignState(k, s).MAC), []byte(s.MAC)) || s.Job != job || s.Nonce != nonce {
		return State{}, errors.New("state failed verification")
	}
	return s, nil
}
