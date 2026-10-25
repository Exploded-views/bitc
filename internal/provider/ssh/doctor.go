package ssh

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
)

// tunnelWant are the values `sshd -T -C user=bitc-tunnel,host=vm,addr=127.0.0.1` must print for
// the controller's tunnel account (providers delta C.6.3).
var tunnelWant = [][2]string{
	{"allowtcpforwarding", "remote"}, {"permitopen", "none"}, {"permitlisten", "127.0.0.1:22022"}, {"gatewayports", "no"},
	{"allowstreamlocalforwarding", "no"}, {"permittunnel", "no"}, {"x11forwarding", "no"}, {"allowagentforwarding", "no"},
	{"permittty", "no"}, {"permituserrc", "no"}, {"forcecommand", "/bin/false"},
}

// CheckTunnelSshd judges the output of `sshd -T` for the tunnel account: one fail line per option
// that is not as C.6.3 wants it, else a single ok line.
func CheckTunnelSshd(out string) []provider.DoctorLine {
	got := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), " "); ok {
			got[strings.ToLower(k)] = strings.TrimSpace(v)
		}
	}
	var lines []provider.DoctorLine
	for _, w := range tunnelWant {
		v, ok := got[w[0]]
		if !ok {
			v = "unset"
		}
		if v != w[1] {
			lines = append(lines, provider.DoctorLine{Level: "fail", Check: "ssh-tunnel-sshd", Detail: fmt.Sprintf("%s is %s, want %s", w[0], v, w[1])})
		}
	}
	if len(lines) == 0 {
		lines = append(lines, provider.DoctorLine{Level: "ok", Check: "ssh-tunnel-sshd", Detail: "bitc-tunnel is limited to listening on 127.0.0.1:22022"})
	}
	return lines
}

func isLoopback(host string) bool { return host == "127.0.0.1" || host == "::1" || host == "localhost" }

// tunnelLines is the C.6.3 check, which only makes sense on the controller of a reverse tunnel
// (host is the controller's own loopback) and needs root there.
func (p *Provider) tunnelLines(ctx context.Context) []provider.DoctorLine {
	if !isLoopback(p.set.Host) {
		return nil
	}
	needsRoot := provider.DoctorLine{Level: "warn", Check: "ssh-tunnel-sshd", Detail: `needs root; run runbook step "tunnel check"`}
	bin := ""
	if t := p.eff.Test; t != nil && t.SSHSshd != "" {
		bin = t.SSHSshd
	} else if t != nil && t.SSHExec == "local" || os.Geteuid() != 0 {
		return []provider.DoctorLine{needsRoot}
	} else if bin, _ = exec.LookPath("sshd"); bin == "" {
		if _, err := os.Stat("/usr/sbin/sshd"); err != nil {
			return []provider.DoctorLine{{Level: "warn", Check: "ssh-tunnel-sshd", Detail: `sshd not found; run runbook step "tunnel check"`}}
		}
		bin = "/usr/sbin/sshd"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-T", "-C", "user=bitc-tunnel,host=vm,addr=127.0.0.1").Output()
	if err != nil {
		return []provider.DoctorLine{{Level: "warn", Check: "ssh-tunnel-sshd", Detail: "sshd -T failed: " + oneLine(err.Error()) + `; run runbook step "tunnel check"`}}
	}
	return CheckTunnelSshd(string(out))
}

// hostKeyFingerprint is the SHA256 fingerprint of a host_key line, as ssh-keygen -l prints it.
func hostKeyFingerprint(key string) string {
	f := strings.Fields(key)
	if len(f) < 2 {
		return ""
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// DoctorLines are the provider's own `doctor --remote` checks (providers delta C.8): the pinned
// key, the host's arch and slots, every item of `check-host --doctor` (relayed as the host prints
// it) and the tunnel account.
func (p *Provider) DoctorLines(ctx context.Context) []provider.DoctorLine {
	if p.credentials() != nil {
		return nil // `ssh api` already fails with the reason
	}
	var out []provider.DoctorLine
	add := func(level, check, detail string) {
		out = append(out, provider.DoctorLine{Level: level, Check: check, Detail: detail})
	}
	add("ok", "ssh-host-key", p.set.HostKeyType+" "+hostKeyFingerprint(p.set.HostKey))
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	host := p.hostLines(ctx)
	if p.upload() {
		var extra []provider.DoctorLine
		host, extra = p.uploadLines(host)
		out = append(out, extra...)
	} else {
		host = slices.DeleteFunc(host, func(l provider.DoctorLine) bool {
			return l.Check == "ssh-git-version" || l.Check == "ssh-git-changelog"
		})
	}
	out = append(out, host...)
	return append(out, p.tunnelLines(ctx)...)
}

func (p *Provider) hostLines(ctx context.Context) []provider.DoctorLine {
	one := func(level, check, detail string) []provider.DoctorLine {
		return []provider.DoctorLine{{Level: level, Check: check, Detail: detail}}
	}
	res, pout, err := p.output(ctx, "probe", nil)
	if err == nil && res.Code != 0 {
		err = p.failure("probe", res, pout)
	}
	if err != nil {
		return one("fail", "ssh-host", unwrapWhy(err))
	}
	var pr struct {
		Arch  string `json:"arch"`
		Slots int    `json:"slots"`
	}
	if err := jsonUnmarshal(pout, &pr); err != nil {
		return one("fail", "ssh-host", "probe: unreadable answer")
	}
	if pr.Arch != p.set.Arch {
		return one("fail", "ssh-arch", fmt.Sprintf("host arch %s, configured %s", pr.Arch, p.set.Arch))
	}
	var lines []provider.DoctorLine
	if pr.Slots < p.set.Slots {
		lines = append(lines, provider.DoctorLine{Level: "fail", Check: "ssh-slots", Detail: fmt.Sprintf("host has %d slots, configured %d", pr.Slots, p.set.Slots)})
	}
	sha, path, err := p.agentFile()
	if err != nil {
		return append(lines, provider.DoctorLine{Level: "fail", Check: "ssh-host", Detail: oneLine(err.Error())})
	}
	for try := 0; ; try++ {
		res, cout, err := p.output(ctx, "agent "+sha+" check-host --doctor", nil)
		if err != nil {
			return append(lines, provider.DoctorLine{Level: "fail", Check: "ssh-host", Detail: unwrapWhy(err)})
		}
		switch {
		case res.Code == 66 && try == 0:
			if err := p.install(ctx, sha, path); err != nil {
				return append(lines, provider.DoctorLine{Level: "fail", Check: "ssh-host", Detail: unwrapWhy(err)})
			}
			continue
		case res.Code == 0 || res.Code == 69:
			parsed := relay(cout)
			if res.Code == 69 && !hasFail(parsed) {
				parsed = append(parsed, provider.DoctorLine{Level: "fail", Check: "ssh-host", Detail: "host check failed: " + firstNonEmpty(lastLine(res.Stderr, cout), "no reason given")})
			}
			return append(lines, parsed...)
		default:
			return append(lines, provider.DoctorLine{Level: "fail", Check: "ssh-host", Detail: unwrapWhy(p.failure("check-host", res, cout))})
		}
	}
}

// unwrapWhy is the reason text of a provider error.
func unwrapWhy(err error) string {
	var un *provider.ErrUnavailable
	var tr *provider.ErrTransient
	var in *provider.ErrIneligible
	switch {
	case errors.As(err, &un):
		return un.Why
	case errors.As(err, &tr):
		return tr.Why
	case errors.As(err, &in):
		return in.Why
	}
	return oneLine(err.Error())
}

func hasFail(ls []provider.DoctorLine) bool {
	for _, l := range ls {
		if l.Level == "fail" {
			return true
		}
	}
	return false
}

// relay turns the lines `check-host` prints ("<level> <id>[: <message>]") into doctor lines.
func relay(out string) []provider.DoctorLine {
	var ls []provider.DoctorLine
	for _, l := range strings.Split(out, "\n") {
		level, rest, ok := strings.Cut(strings.TrimSpace(l), " ")
		if !ok || (level != "ok" && level != "warn" && level != "fail") {
			continue
		}
		id, msg, _ := strings.Cut(rest, ": ")
		ls = append(ls, provider.DoctorLine{Level: level, Check: strings.TrimSuffix(id, ":"), Detail: oneLine(msg)})
	}
	return ls
}
