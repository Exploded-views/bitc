package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/provider"
)

// BootPayload is what stage 1 hands stage 2 on fd 3 (spec 8.4 step 1). The environment path
// builds it from BITC_*; --boot-stdin reads it from a length-prefixed frame on stdin.
type BootPayload struct {
	Spec        string `json:"spec"`
	Deadline    int64  `json:"deadline"`
	DeployKey   string `json:"deploy_key"`
	Provider    string `json:"provider"`
	ResourceID  string `json:"resource_id"`
	ScopedKey   string `json:"scoped_key"`
	APIBase     string `json:"api_base"`
	ResultsRoot string `json:"results_root,omitempty"` // --boot-stdin only; default /run/bitc
	// KeepAlive asks for a Tasks API task held for the job (--boot-stdin on Sprites only).
	KeepAlive *KeepAlive  `json:"keep_alive,omitempty"`
	Code      *CodeUpload `json:"code,omitempty"`
}

// KeepAlive is the lifetime of the keep-alive task and how often it is renewed.
type KeepAlive struct {
	Expire Duration `json:"expire"`
	Renew  Duration `json:"renew"`
}

// Main is `bitc __agent`: stage 1 moves secrets out of the env and re-execs; stage 2 runs.
func Main(args []string, stdout io.Writer) int { return MainIO(args, os.Stdin, stdout) }

// MainIO is Main with the input and the error stream explicit. Besides the environment boot it
// runs `--boot-stdin` (a length-prefixed payload on stdin, no listener) and the exec results
// verbs `read`, `tail`, `list` and `teardown` (providers delta X.1).
func MainIO(args []string, stdin io.Reader, stdout io.Writer) int {
	// Whatever umask we were started under (the build stub's 077 protects the deploy key), the
	// dirs the job user must traverse are 0755; results dirs chmod 0700 explicitly.
	unix.Umask(0o022)
	if len(args) == 1 && args[0] == "--stage2" {
		return stage2(stdout)
	}
	if len(args) == 1 && args[0] == "--stage2-stdin" {
		return stage2Stdin(stdout)
	}
	if len(args) == 1 && args[0] == "--boot-stdin" {
		return bootStdin(stdin, stdout)
	}
	if len(args) == 1 && args[0] == "provision" {
		return provisionVerb(stdin, stdout)
	}
	if len(args) > 0 {
		if code, ok := hostVerb(args, stdin, stdout); ok {
			return code
		}
	}
	if isExecVerb(args) {
		return ExecVerb(args, stdout, os.Stderr)
	}
	spec, err := DecodeSpec(os.Getenv("BITC_SPEC"))
	if err != nil {
		fmt.Fprintln(stdout, "bitc agent: no usable BITC_SPEC")
		return 69
	}
	addr := ":" + strconv.Itoa(spec.Port)
	fakePod := spec.Unprivileged && spec.TestRoot != "" && spec.Port == 0
	if fakePod {
		addr = "127.0.0.1:0" // a fake pod (the RunPod fake): any free loopback port
	}
	ln, err := net.Listen("tcp", addr) // bound before any package or repo code
	if err != nil {
		fmt.Fprintln(stdout, "bitc agent: cannot bind the results port:", err)
		return 69
	}
	if fakePod {
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		if os.MkdirAll(spec.TestRoot, 0o700) != nil || os.WriteFile(filepath.Join(spec.TestRoot, "port"), []byte(port), 0o600) != nil {
			fmt.Fprintln(stdout, "bitc agent: cannot record the fake pod's port")
			return 69
		}
	}
	sys := System(RealSystem{})
	if spec.Unprivileged && spec.TestRoot != "" {
		sys = PlainSystem{}
	}
	_ = sys.SetOOMScoreAdj()
	dl, _ := strconv.ParseInt(os.Getenv("BITC_DEADLINE"), 10, 64)
	p := BootPayload{Spec: os.Getenv("BITC_SPEC"), Deadline: dl, DeployKey: os.Getenv("BITC_DEPLOY_KEY"),
		Provider: os.Getenv("BITC_PROVIDER_TYPE"), ResourceID: os.Getenv("RUNPOD_POD_ID"), ScopedKey: os.Getenv("RUNPOD_API_KEY"), APIBase: os.Getenv("BITC_API_BASE")}
	b, _ := json.Marshal(p)
	// MFD_CLOEXEC: the original never survives the exec; only the dup on fd 3 does.
	fd, err := unix.MemfdCreate("bitc-boot", unix.MFD_CLOEXEC)
	if err != nil {
		return 69
	}
	if _, err := unix.Write(fd, b); err != nil {
		return 69
	}
	_, _ = unix.Seek(fd, 0, 0)
	lf, err := ln.(*net.TCPListener).File()
	if err != nil {
		return 69
	}
	// Park both well above 3 and 4 first, so neither can sit on the other's target; then
	// Dup3 with flags 0 clears CLOEXEC on exactly fd 3 (payload) and fd 4 (listener).
	pay, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 10)
	if err != nil {
		return 69
	}
	lis, err := unix.FcntlInt(lf.Fd(), unix.F_DUPFD_CLOEXEC, 10)
	if err != nil {
		return 69
	}
	if err := unix.Dup3(pay, 3, 0); err != nil {
		return 69
	}
	if err := unix.Dup3(lis, 4, 0); err != nil {
		return 69
	}
	self, _ := os.Readlink("/proc/self/exe")
	return toErr(syscall.Exec(self, []string{self, "__agent", "--stage2"}, []string{}))
}

func toErr(err error) int { fmt.Fprintln(os.Stderr, "bitc agent: re-exec:", err); return 69 }

func stage2(stdout io.Writer) int {
	f := os.NewFile(3, "boot")
	b, _ := io.ReadAll(f)
	f.Close()
	var p BootPayload
	if json.Unmarshal(b, &p) != nil {
		return 69
	}
	lf := os.NewFile(4, "listener")
	ln, err := net.FileListener(lf) // dups fd 4
	lf.Close()                      // the dup is close-on-exec; fd 4 must not reach any child
	if err != nil {
		return 69
	}
	return runStage2(stdout, p, ln)
}

// runStage2 sets up the paths for the real agent (or, for a test spec, a plain-user one under its
// TestRoot) and runs the job. ln is nil in no-listener mode.
func runStage2(stdout io.Writer, p BootPayload, ln net.Listener) int {
	spec, err := DecodeSpec(p.Spec)
	if err != nil {
		return 69
	}
	o := Options{Stdout: stdout, ResultsRoot: "/run/bitc", ToolsRoot: "/opt/bitc/tools", WorkRoot: "/work",
		System: RealSystem{}, Listener: ln, Deadline: time.Unix(p.Deadline, 0)}
	if p.ResultsRoot != "" {
		o.ResultsRoot = p.ResultsRoot
	}
	if spec.Unprivileged && spec.TestRoot != "" {
		o.System = PlainSystem{}
		o.ResultsRoot, o.ToolsRoot, o.WorkRoot = spec.TestRoot+"/run", spec.TestRoot+"/tools", spec.TestRoot+"/work"
	}
	if spec.Port == -1 { // exec delivery: the marker, the key tmpfs and the keep-alive task live in fixed places
		o.StateRoot, o.KeyRoot = "/var/lib/bitc", "/run/bitc-key"
		sock := "/.sprite/api.sock"
		if spec.Unprivileged && spec.TestRoot != "" {
			o.StateRoot, o.KeyRoot = spec.TestRoot+"/var/lib/bitc", filepath.Join(o.ResultsRoot, ".key")
			sock = spec.TestRoot + sock
			if p.Code != nil { // the test host's root side left the bundle under the test root's work dir
				o.Code, o.UploadLock = p.Code, filepath.Join(o.StateRoot, "upload.lock")
				_ = os.MkdirAll(o.StateRoot, 0o700)
			}
		}
		if k := p.KeepAlive; k != nil {
			o.KeepAlive = &KeepAliveConf{Sock: sock, Expire: time.Duration(k.Expire), Renew: time.Duration(k.Renew)}
		}
	}
	if fac, ok := provider.Get(p.Provider); ok && fac.SelfDelete != nil {
		o.SelfDelete = selfDeleteHook(spec, p, fac.SelfDelete, stdout)
	}
	return Run(context.Background(), o, spec, Secrets{DeployKey: p.DeployKey})
}

// selfDeleteHook builds the D + 10m hook. The API base comes from the create env
// (BITC_API_BASE, set by the provider from its configured base). In a test run (unprivileged
// with a test root) a base that is not loopback fails closed: no request is made.
func selfDeleteHook(spec Spec, p BootPayload, del func(context.Context, provider.SelfDeleteEnv) error, log io.Writer) func(context.Context) error {
	env := provider.SelfDeleteEnv{ResourceID: p.ResourceID, ScopedKey: p.ScopedKey, APIBase: p.APIBase}
	testMode := spec.Unprivileged && spec.TestRoot != ""
	return func(ctx context.Context) error {
		if testMode && !loopbackBase(env.APIBase) {
			fmt.Fprintln(log, "bitc agent: test mode: refusing self-delete against a non-loopback API base")
			return errors.New("self-delete refused: non-loopback API base in test mode")
		}
		return del(ctx, env)
	}
}

// loopbackBase reports whether base is an http(s) URL whose host is localhost or a loopback IP.
func loopbackBase(base string) bool {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
