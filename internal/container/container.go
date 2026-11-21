// Package container runs a local pool's command in a container (docs/containers.md): the docker
// run argv, the labels that tie a container to the ticket that started it, the guard process that
// removes a container when bitc is killed, and the sweep that removes orphans by label.
package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
)

// The labels on every container bitc starts. bitc.state scopes a sweep to one state dir;
// bitc.ticket (the ticket file name without .json) is what tells a live run from an orphan.
const (
	LabelJob    = "bitc.job"
	LabelPool   = "bitc.pool"
	LabelState  = "bitc.state"
	LabelTicket = "bitc.ticket"
)

// removeLimit bounds one `docker rm -f` or `docker ps`, so a hung daemon cannot hold bitc.
const removeLimit = 30 * time.Second

// Name is the container's name, unique per run: bitc-<ticket id>. The job name is only a label
// (bitc.job): --name is the caller's and may be reused, even by runs going at the same time, and a
// removal by a name two runs share would remove the other run's container.
func Name(ticket string) string { return "bitc-" + ticket }

// StateHash is the bitc.state label: the first 12 hex digits of the sha256 of the state dir path.
func StateHash(stateDir string) string {
	sum := sha256.Sum256([]byte(stateDir))
	return hex.EncodeToString(sum[:])[:12]
}

// TicketID is a ticket file's name without ".json".
func TicketID(ticketFile string) string { return strings.TrimSuffix(ticketFile, ".json") }

// ErrNotFound is a runtime that is not on PATH.
var ErrNotFound = errors.New("not found on PATH")

// Lookup finds the runtime's CLI on the PATH of env (the run's environment, last PATH wins).
func Lookup(name string, env []string) (string, error) {
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			if abs, err := filepath.Abs(p); err == nil {
				return abs, nil
			}
			return p, nil
		}
	}
	return "", ErrNotFound
}

// Spec is one container run.
type Spec struct {
	Runtime string // the CLI's path (Lookup)
	Image   string
	Job     string
	Pool    string
	State   string // StateHash
	Ticket  string // TicketID
	CID     string // --cidfile path
	Mounts  []config.Mount
	Dir     string // --workdir
	CPUs    string // "" = no limit
	Memory  config.Size
	EnvFile string // --env-file path (WriteEnvFile): the container's environment, never on argv
	TTY     bool
	Argv    []string // the command
}

// RunArgv is the whole `docker run` command line, program first.
func RunArgv(s Spec) []string {
	tty := "-i"
	if s.TTY {
		tty = "-it"
	}
	a := []string{s.Runtime, "run", "--rm", "--init", tty, "--name", Name(s.Ticket),
		"--label", LabelJob + "=" + s.Job, "--label", LabelPool + "=" + s.Pool,
		"--label", LabelState + "=" + s.State, "--label", LabelTicket + "=" + s.Ticket,
		"--cidfile", s.CID}
	if s.EnvFile != "" {
		a = append(a, "--env-file", s.EnvFile)
	}
	for _, m := range s.Mounts {
		v := "type=bind,source=" + m.Source + ",target=" + m.Target
		if m.ReadOnly {
			v += ",readonly"
		}
		a = append(a, "--mount", v)
	}
	a = append(a, "--workdir", s.Dir)
	if s.CPUs != "" {
		a = append(a, "--cpus", s.CPUs)
	}
	if s.Memory > 0 {
		a = append(a, "--memory", strconv.FormatInt(int64(s.Memory), 10))
	}
	a = append(a, s.Image)
	return append(a, s.Argv...)
}

// WriteEnvFile writes env (NAME=VALUE entries) one per line to path, mode 0600, for --env-file. It
// replaces a leftover of the same name, and writes nothing if an entry cannot be one line of the
// file (no name, no =, a newline or a NUL in it). The state dir around it is 0700 as well.
func WriteEnvFile(path string, env []string) error {
	var b strings.Builder
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.ContainsAny(kv, "\n\r\x00") {
			return fmt.Errorf("%q cannot be a line of an env file", k)
		}
		b.WriteString(kv + "\n")
	}
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.WriteString(b.String())
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

// fullIDRE is a full container id: 64 lowercase hex digits.
var fullIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ReadID is the container id docker wrote to the cidfile, and only a full one (64 hex digits): ""
// if there is no file, nothing in it, or anything else. No id means docker never created a
// container for this run, and nothing is removed: a short id would be resolved by the daemon as a
// name first and a prefix of another container's id next, which is not "this run's container".
func ReadID(cidfile string) string {
	b, err := os.ReadFile(cidfile) //nolint:gosec // G703: a path bitc built in its own state dir
	if err != nil {
		return ""
	}
	if id := strings.TrimSpace(string(b)); fullIDRE.MatchString(id) {
		return id
	}
	return ""
}

// RemoveArgv is the forced removal of the container with this id.
func RemoveArgv(runtime, id string) []string { return []string{runtime, "rm", "-f", id} }

// Remove force-removes the container with this id (`rm -f`). It is by id and never by name: an id
// is the one container this run (or the listing) saw, a name can be another run's. A container
// that is already gone is not an error: the run's own --rm, the guard and the sweep may all get
// there first.
func Remove(ctx context.Context, runtime, id string, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, removeLimit)
	defer cancel()
	argv := RemoveArgv(runtime, id)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") {
		return errors.New(firstLine(string(out), err))
	}
	return nil
}

func firstLine(out string, err error) string {
	out = strings.TrimSpace(out)
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	if out == "" {
		return err.Error()
	}
	return out
}
