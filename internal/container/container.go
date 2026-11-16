// Package container runs a local pool's command in a container (docs/containers.md): the docker
// run argv, the labels that tie a container to the ticket that started it, the guard process that
// removes a container when bitc is killed, and the sweep that removes orphans by label.
package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// Name is the container's name: deterministic from the job.
func Name(job string) string { return "bitc-" + job }

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
	Env     []string // NAME=VALUE, in the order they are passed
	TTY     bool
	Argv    []string // the command
}

// RunArgv is the whole `docker run` command line, program first.
func RunArgv(s Spec) []string {
	tty := "-i"
	if s.TTY {
		tty = "-it"
	}
	a := []string{s.Runtime, "run", "--rm", "--init", tty, "--name", Name(s.Job),
		"--label", LabelJob + "=" + s.Job, "--label", LabelPool + "=" + s.Pool,
		"--label", LabelState + "=" + s.State, "--label", LabelTicket + "=" + s.Ticket,
		"--cidfile", s.CID}
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
	for _, e := range s.Env {
		a = append(a, "-e", e)
	}
	a = append(a, s.Image)
	return append(a, s.Argv...)
}

// RemoveArgv is the forced removal of the named container.
func RemoveArgv(runtime, name string) []string { return []string{runtime, "rm", "-f", name} }

// Remove force-removes the container (`rm -f`). A container that is already gone is not an error:
// the run's own --rm, the guard and the sweep may all get there first.
func Remove(ctx context.Context, runtime, name string, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, removeLimit)
	defer cancel()
	argv := RemoveArgv(runtime, name)
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
