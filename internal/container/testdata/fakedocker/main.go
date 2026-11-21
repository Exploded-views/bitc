// Command fakedocker is a stand-in docker CLI for bitc's tests: no daemon, no images. It records
// every call's argv, runs the "container" command on the host (alone in its own process group,
// with only the -e variables and PATH), and implements the three calls bitc makes:
//
//	run [options] IMAGE COMMAND...   start the command; --rm, --name, --label, --cidfile and -e are honoured
//	rm -f NAME...                    kill the container's process group and forget it
//	ps -a --filter label=K=V... --format F   list the known containers whose labels match
//
// State lives in $FAKE_DOCKER_DIR: argv.log (one JSON array per call) and c/<name>.json (one file
// per container: its labels and the pid/process group of its command). `-e NAME` takes the value
// from the fake's own environment, as the real CLI does.
//
// Signals: the real `docker run` proxies INT, TERM and HUP to the container unless it has a TTY
// (-t) or --sig-proxy=false. The fake does NOT, by default: that is the worst case, where a TERM or
// a SIGKILL that reaches only the CLI leaves the container running until `rm -f`. FAKE_DOCKER_PROXY=1
// makes it proxy like the real one (still not with -t).
//
// Not modelled: the daemon-side auto-remove (since API 1.25 a --rm container whose CLI was killed
// is still removed by the daemon when its command exits); the fake removes it when the CLI ends or
// on `rm -f`. bitc's `rm -f` is idempotent, so the difference is not a bitc correctness matter.
//
// FAKE_DOCKER_FAIL=run|rm|ps makes that call fail at once like a daemon error (exit 125, nothing
// created or removed; the call is still logged).
//
// Ids are 64 hex digits. `ps` prints the 12-digit truncated id unless --no-trunc, as docker does,
// and `rm` resolves its argument as a full id, then a name, then an id prefix, as the daemon does.
// An --env-file with a line over 64 KiB or invalid UTF-8 makes `run` fail (125), as docker's parser
// does. Every call also logs the DOCKER_HOST it was run with (hostenv.log), to show which
// environment bitc gave the CLI.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/Exploded-views/bitc/internal/exit"
)

// envSnapshot is what the fake saw of an --env-file when `run` started (the file is bitc's to
// remove, and the tests look at it afterwards): its path, mode and body.
type envSnapshot struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Body string `json:"body"`
}

type record struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Pgid   int               `json:"pgid"`
}

var dir = os.Getenv("FAKE_DOCKER_DIR")

func main() {
	if dir == "" {
		fmt.Fprintln(os.Stderr, "fakedocker: FAKE_DOCKER_DIR is not set")
		os.Exit(125)
	}
	_ = os.MkdirAll(filepath.Join(dir, "c"), 0o755)
	logCall(os.Args[1:])
	if f, err := os.OpenFile(filepath.Join(dir, "hostenv.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString(os.Getenv("DOCKER_HOST") + "\n")
		f.Close()
	}
	if len(os.Args) < 2 {
		os.Exit(125)
	}
	if f := os.Getenv("FAKE_DOCKER_FAIL"); f != "" && f == os.Args[1] {
		fmt.Fprintf(os.Stderr, "Cannot connect to the Docker daemon at unix:///fake.sock. Is the docker daemon running? (%s)\n", f)
		os.Exit(125)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(run(os.Args[2:]))
	case "rm":
		os.Exit(rm(os.Args[2:]))
	case "ps":
		os.Exit(ps(os.Args[2:]))
	}
	fmt.Fprintf(os.Stderr, "fakedocker: unsupported command %q\n", os.Args[1])
	os.Exit(125)
}

func logCall(args []string) {
	b, _ := json.Marshal(args)
	f, err := os.OpenFile(filepath.Join(dir, "argv.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n')) // one write: concurrent calls do not interleave
}

func recPath(name string) string { return filepath.Join(dir, "c", name+".json") }

func writeRec(r record) {
	b, _ := json.Marshal(r)
	tmp := recPath(r.Name) + ".tmp"
	_ = os.WriteFile(tmp, b, 0o644)
	_ = os.Rename(tmp, recPath(r.Name))
}

func readRec(name string) (record, bool) {
	var r record
	b, err := os.ReadFile(recPath(name))
	if err != nil || json.Unmarshal(b, &r) != nil {
		return r, false
	}
	return r, true
}

func run(args []string) int {
	var name, cidfile, workdir, envfile string
	var env []string
	labels := map[string]string{}
	rmAfter, tty := false, false
	valued := map[string]bool{"--name": true, "--label": true, "--cidfile": true, "--mount": true, "--workdir": true, "-w": true,
		"--cpus": true, "--memory": true, "-e": true, "--env": true, "--env-file": true, "-v": true, "--user": true, "-u": true}
	flags := map[string]bool{"--rm": true, "--init": true, "-i": true, "-t": true, "-it": true, "-ti": true}
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		a := args[i]
		switch {
		case flags[a]:
			rmAfter = rmAfter || a == "--rm"
			tty = tty || a == "-t" || a == "-it" || a == "-ti"
		case valued[a] && i+1 < len(args):
			i++
			v := args[i]
			switch a {
			case "--name":
				name = v
			case "--label":
				k, val, _ := strings.Cut(v, "=")
				labels[k] = val
			case "--cidfile":
				cidfile = v
			case "--env-file":
				envfile = v
			case "--workdir", "-w":
				workdir = v
			case "-e", "--env":
				if !strings.Contains(v, "=") { // -e NAME: the value is the CLI's own
					v += "=" + os.Getenv(v)
				}
				env = append(env, v)
			}
		default:
			fmt.Fprintf(os.Stderr, "fakedocker: unknown flag %q\n", a)
			return 125
		}
	}
	if i >= len(args) {
		fmt.Fprintln(os.Stderr, "docker run: requires at least 1 argument")
		return 125
	}
	cmdArgs := args[i+1:] // args[i] is the image
	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "fakedocker: the fake needs a command")
		return 125
	}
	if name == "" {
		name = fmt.Sprintf("fake-%d", os.Getpid())
	}
	if _, ok := readRec(name); ok {
		fmt.Fprintf(os.Stderr, "docker: Error response from daemon: Conflict. The container name \"/%s\" is already in use.\n", name)
		return 125
	}
	if envfile != "" { // docker reads it first; an unreadable one is a failure before the container exists
		st, err := os.Stat(envfile)
		b, rerr := os.ReadFile(envfile)
		if err != nil || rerr != nil {
			fmt.Fprintf(os.Stderr, "docker: open %s: no such file or directory\n", envfile)
			return 125
		}
		snap, _ := json.Marshal(envSnapshot{Path: envfile, Mode: fmt.Sprintf("%04o", st.Mode().Perm()), Body: string(b)})
		_ = os.MkdirAll(filepath.Join(dir, "envfiles"), 0o755)
		_ = os.WriteFile(filepath.Join(dir, "envfiles", name+".json"), snap, 0o644)
		for _, l := range strings.Split(string(b), "\n") {
			if len(l) >= 64*1024 || !utf8.ValidString(l) { // docker's parser: a bufio.Scanner line, and valid UTF-8
				fmt.Fprintf(os.Stderr, "docker: read %s: bufio.Scanner: token too long or invalid UTF-8\n", envfile)
				return 125
			}
			switch {
			case l == "" || strings.HasPrefix(l, "#"):
			case strings.Contains(l, "="):
				env = append(env, l)
			default: // NAME alone: the value is the CLI's own
				env = append(env, l+"="+os.Getenv(l))
			}
		}
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())))
	id := hex.EncodeToString(sum[:])
	if cidfile != "" {
		f, err := os.OpenFile(cidfile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "docker: Container ID file found, make sure the other container isn't running or delete %s\n", cidfile)
			return 125
		}
		_, _ = f.WriteString(id)
		f.Close()
	}
	rec := record{ID: id, Name: name, Labels: labels}
	writeRec(rec)
	if f, err := os.OpenFile(filepath.Join(dir, "created.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString(name + "\n")
		f.Close()
	}
	if rmAfter {
		defer os.Remove(recPath(name))
	}
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	cmd.Dir = workdir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var sigs chan os.Signal
	if os.Getenv("FAKE_DOCKER_PROXY") == "1" && !tty {
		sigs = make(chan os.Signal, 8)
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "docker: Error response from daemon: exec: %q: executable file not found\n", cmdArgs[0])
		return 127
	}
	rec.Pgid = cmd.Process.Pid
	writeRec(rec)
	if sigs != nil {
		exit.Go(func() {
			for s := range sigs {
				_ = syscall.Kill(-rec.Pgid, s.(syscall.Signal))
			}
		})
	}
	err := cmd.Wait()
	if cmd.ProcessState == nil {
		fmt.Fprintln(os.Stderr, err)
		return 125
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

// find resolves a `docker rm` argument as the daemon does: a full id, then a name, then an id prefix.
func find(arg string) (record, bool) {
	var all []record
	ents, _ := os.ReadDir(filepath.Join(dir, "c"))
	for _, e := range ents {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			if r, ok := readRec(name); ok {
				all = append(all, r)
			}
		}
	}
	for _, r := range all {
		if r.ID == arg {
			return r, true
		}
	}
	for _, r := range all {
		if r.Name == arg {
			return r, true
		}
	}
	for _, r := range all {
		if arg != "" && strings.HasPrefix(r.ID, arg) {
			return r, true
		}
	}
	return record{}, false
}

func rm(args []string) int {
	rc := 0
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		r, ok := find(a)
		if !ok {
			fmt.Fprintf(os.Stderr, "Error response from daemon: No such container: %s\n", a)
			rc = 1
			continue
		}
		if r.Pgid > 0 {
			_ = syscall.Kill(-r.Pgid, syscall.SIGKILL)
		}
		_ = os.Remove(recPath(r.Name))
		b, _ := json.Marshal(map[string]string{"arg": a, "name": r.Name, "id": r.ID})
		if f, err := os.OpenFile(filepath.Join(dir, "removed.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.Write(append(b, '\n'))
			f.Close()
		}
		fmt.Println(a)
	}
	return rc
}

var labelRE = regexp.MustCompile(`\{\{\s*\.Label\s+"([^"]*)"\s*\}\}`)

func ps(args []string) int {
	want := map[string]string{}
	format := "{{.Names}}"
	noTrunc := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--no-trunc":
			noTrunc = true
		case "--filter", "-f":
			i++
			if i < len(args) {
				if kv, ok := strings.CutPrefix(args[i], "label="); ok {
					k, v, _ := strings.Cut(kv, "=")
					want[k] = v
				}
			}
		case "--format":
			i++
			if i < len(args) {
				format = args[i]
			}
		}
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "c"))
next:
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		r, ok := readRec(name)
		if !ok {
			continue
		}
		for k, v := range want {
			if r.Labels[k] != v {
				continue next
			}
		}
		id := r.ID
		if !noTrunc && len(id) > 12 {
			id = id[:12] // docker truncates the id in a listing unless told not to
		}
		line := strings.ReplaceAll(strings.ReplaceAll(format, "{{.Names}}", r.Name), "{{.ID}}", id)
		line = labelRE.ReplaceAllStringFunc(line, func(m string) string { return r.Labels[labelRE.FindStringSubmatch(m)[1]] })
		fmt.Println(line)
	}
	return 0
}
