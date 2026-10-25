package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Fault modes are for the conformance suites, not for plugin authors to copy. They come from the
// `fault` key of the host section (a comma list, known from `configure` on) and from the
// environment variable FAKEX_FAULT (known from the start, so `hello` can show one; the host must
// list it in plugin.env_pass). They are:
//
//	extra-fields                  unknown fields in every result, event and error (and hello)
//	hello-no-caps                 hello without caps; configure-no-endpoints: configure without endpoints
//	estimate-zero|-negative|-float   an estimate of 0, -5 or 1.5
//	list-omits|-foreign-name|-negative-price|-not-array|-fails|-fails-after-create   list shows nothing, a foreign name, a negative price, a non-array, or fails (always, or once this process has created an instance)
//	echo-launch                   copy each launch request to stderr
//	create-then-transient-no-id   create the instance, then answer transient without an id (a plugin bug)
//	launch-transient-nothing      answer transient without creating anything
//	launch-malformed              after a successful launch, write a broken frame and fall silent
//	watch-no-exit                 a watch result without exit
//	preflight-ineligible, teardown-fails   what they say
//	hang:<method>                 never answer <method>
//	diag                          write diag-<pid>.json (env, argv, no-new-privs, fd 3 secrets) to BITC_PLUGIN_STATE
func parseFaults(list string) map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Split(list, ",") {
		if f = strings.TrimSpace(f); f != "" {
			m[f] = true
		}
	}
	return m
}

func (s *server) fault(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.faults[name]
}

// decorate adds a field no reader knows to a result, event or error when the extra-fields fault is
// on: bitc must ignore it (A.4.6).
func (s *server) decorate(v any) any {
	if !s.fault("extra-fields") {
		return v
	}
	b, err := json.Marshal(v)
	var m map[string]any
	if err != nil || json.Unmarshal(b, &m) != nil {
		return v
	}
	m["future"] = map[string]any{"x": 1}
	return m
}

// rawResult is a result frame body the fault writes verbatim.
type rawResult string

func (r rawResult) MarshalJSON() ([]byte, error) { return []byte(r), nil }

// diag records facts about this process for the conformance suite (fault diag): the environment,
// argv, whether no-new-privs is set, and the secrets read from fd 3. A real plugin never writes
// its secrets anywhere.
func (s *server) diag() {
	if !s.fault("diag") {
		return
	}
	d := map[string]any{"env": os.Environ(), "args": os.Args, "secrets": s.secrets, "pid": os.Getpid()}
	if st, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, l := range strings.Split(string(st), "\n") {
			if v, ok := strings.CutPrefix(l, "NoNewPrivs:"); ok {
				d["nonewprivs"] = strings.TrimSpace(v)
			}
		}
	}
	if b, err := os.ReadFile("/proc/self/cmdline"); err == nil {
		d["cmdline"] = strings.ReplaceAll(string(b), "\x00", " ")
	}
	var fds []string
	if ents, err := os.ReadDir("/proc/self/fd"); err == nil {
		for _, e := range ents {
			l, _ := os.Readlink("/proc/self/fd/" + e.Name())
			fds = append(fds, e.Name()+" "+l)
		}
		sort.Strings(fds)
	}
	d["fds"] = fds
	b, _ := json.Marshal(d)
	_ = os.WriteFile(filepath.Join(os.Getenv("BITC_PLUGIN_STATE"), fmt.Sprintf("diag-%d.json", os.Getpid())), b, 0o600) //nolint:gosec // BITC_PLUGIN_STATE is the directory bitc made for this plugin
}
