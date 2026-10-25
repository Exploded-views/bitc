//go:build privileged

package conformance

// SH38 on the privileged rig (a real sshd and the root-side agent): an upload job runs from the
// bundle that follows its frame on the gate session, and a job whose bundle does not match the
// payload's sha256 is refused and leaves no .bitc-upload. Run by the controller through the
// privileged workflow, never by an agent.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
)

// uploadFrame is the boot frame of an upload job for the rig's commit, and the bundle that follows it.
func (j *rigJob) uploadFrame(t *testing.T, script, sum string) (frame, bundle []byte) {
	t.Helper()
	ref := "refs/bitc/upload/" + j.ID
	path := "/srv/bitc-rig/" + j.ID + ".bundle"
	if out, err := rigRun("set -e; cd /srv/bitc-rig/src; git update-ref " + ref + " HEAD; git bundle create " + path + " " + ref + "; git update-ref -d " + ref); err != nil {
		t.Fatalf("bundle: %s", out)
	}
	bundle, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sum == "" {
		s := sha256.Sum256(bundle)
		sum = hex.EncodeToString(s[:])
	}
	spec := agent.Spec{Job: j.ID, Pool: "heavy", Argv: []string{"/bin/sh", "-c", script}, Sha: j.r.RepoSha, Ref: ref,
		Timeout: agent.Duration(2 * time.Minute), Port: -1, Bearer: strings.Repeat("04", 32), MacKey: strings.Repeat("05", 32), Nonce: "n0nce"}
	enc, err := spec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	frame, err = agent.BootPayload{Spec: enc, Deadline: time.Now().Add(10 * time.Minute).Unix(),
		Code: &agent.CodeUpload{Kind: "upload", Len: int64(len(bundle)), SHA256: sum, Ref: ref}}.Frame()
	if err != nil {
		t.Fatal(err)
	}
	return frame, bundle
}

func TestSH38_UploadJobRunsOnARealSSHDAndAHashMismatchIsRefusedWithNoFile(t *testing.T) {
	r := rig(t)

	// one upload job runs: no clone URL, no deploy key, the file comes out of the bundle
	j, out, errOut, code := r.prepare("1", "")
	if code != 0 || out != "prepared "+j.Name+"\n" {
		t.Fatalf("prepare: %q %q code %d", out, errOut, code)
	}
	defer j.teardown()
	frame, bundle := j.uploadFrame(t, "cat file", "")
	if out, errOut, code := r.agentCtl("slot-boot --run 1 "+j.Name+" "+j.Nonce, append(frame, bundle...)); code != 0 || out != "started "+j.Name+"\n" {
		t.Fatalf("--run with a bundle: %q %q code %d", out, errOut, code)
	}
	if res := j.wait(t); res.Out != "hi\n" || res.Exit != 0 {
		t.Errorf("upload job output %q exit %d, want \"hi\\n\" and 0 (err %q)", res.Out, res.Exit, res.Err)
	}
	if exists("/var/lib/bitc/slots/1/work/.bitc-upload") {
		t.Error("the bundle is still on the host after the job")
	}

	// a bundle that does not hash to the payload's sha256 is refused and leaves no file
	k, out, errOut, code := r.prepare("2", "")
	if code != 0 || out != "prepared "+k.Name+"\n" {
		t.Fatalf("prepare slot 2: %q %q code %d", out, errOut, code)
	}
	defer k.teardown()
	frame, bundle = k.uploadFrame(t, "cat file", strings.Repeat("0", 64))
	if out, _, code := r.agentCtl("slot-boot --run 2 "+k.Name+" "+k.Nonce, append(frame, bundle...)); code != 65 || out != "refused: bundle hash mismatch\n" {
		t.Errorf("a mismatching bundle: %q code %d, want the hash refusal and 65", out, code)
	}
	if exists("/var/lib/bitc/slots/2/work/.bitc-upload") {
		t.Error("a refused upload left .bitc-upload behind")
	}
}
