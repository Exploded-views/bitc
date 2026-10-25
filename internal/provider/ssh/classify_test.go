package ssh

import "testing"

const (
	wlRefused = "ssh: connect to host 127.0.0.1 port 22022: Connection refused"
	wlTimeout = "ssh: connect to host 127.0.0.1 port 22022: Connection timed out"
)

// The pre-session whitelist is matched exactly, line for line (providers delta C.5.1 item 3).
func TestClassifySessionWhitelist(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		stderr string
		want   Class
	}{
		{"refused", 255, wlRefused + "\n", ClassConnect},
		{"timed out", 255, wlTimeout + "\n", ClassConnect},
		{"no route", 255, "ssh: connect to host 127.0.0.1 port 22022: No route to host\n", ClassConnect},
		{"unreachable", 255, "ssh: connect to host 127.0.0.1 port 22022: Network is unreachable\n", ClassConnect},
		{"resolve", 255, "ssh: Could not resolve hostname 127.0.0.1: Name or service not known\n", ClassConnect},
		{"kex", 255, "kex_exchange_identification: read: Connection reset by peer\n", ClassConnect},
		{"kex closed", 255, "kex_exchange_identification: Connection closed by remote host\n", ClassConnect},
		{"host key", 255, "Host key verification failed.\n", ClassHostKey},
		{"auth", 255, "bitc@127.0.0.1: Permission denied (publickey).\n", ClassAuth},
		{"no trailing newline", 255, wlRefused, ClassConnect},
		{"other host", 255, "ssh: connect to host 10.0.0.9 port 22022: Connection refused\n", ClassOther},
		{"wrong user", 255, "root@127.0.0.1: Permission denied (publickey).\n", ClassOther},
		{"two lines", 255, wlRefused + "\n" + wlRefused + "\n", ClassOther},
		{"extra text", 255, wlRefused + " (extra)\n", ClassOther},
		{"other 255 text", 255, "client_loop: send disconnect: Broken pipe\n", ClassOther},
		{"empty 255", 255, "", ClassOther},
		{"exit 1", 1, wlRefused + "\n", ClassOther},
		{"exit 0", 0, wlRefused + "\n", ClassOther},
		{"warning block", 255, "@@@@\nWARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!\nHost key verification failed.\n", ClassOther},
	}
	for _, c := range cases {
		got, why := ClassifySession(c.code, c.stderr, "bitc", "127.0.0.1")
		if got != c.want {
			t.Errorf("%s: class %v, want %v", c.name, got, c.want)
		}
		if got != ClassOther && why == "" {
			t.Errorf("%s: no reason", c.name)
		}
	}
}

func TestClassifySessionReasonsNameTheCause(t *testing.T) {
	_, why := ClassifySession(255, wlTimeout, "bitc", "127.0.0.1")
	if why != "connect timed out" {
		t.Errorf("timeout: %q", why)
	}
	_, why = ClassifySession(255, wlRefused, "bitc", "127.0.0.1")
	if why != "connection refused" {
		t.Errorf("refused: %q", why)
	}
	_, why = ClassifySession(255, "Host key verification failed.", "bitc", "127.0.0.1")
	if why != "host key verification failed (the pinned host_key does not match)" {
		t.Errorf("host key: %q", why)
	}
	_, why = ClassifySession(255, "bitc@127.0.0.1: Permission denied (publickey).", "bitc", "127.0.0.1")
	if why != "permission denied (publickey)" {
		t.Errorf("auth: %q", why)
	}
}

// Before the resource point nothing ran on the host, so a looser reading is safe: the line may
// sit among others (the host-key mismatch prints a banner).
func TestClassifyLooseFindsTheLineAmongOthers(t *testing.T) {
	c, _ := ClassifyLoose(255, "@@@@\nWARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!\nHost key verification failed.\n")
	if c != ClassHostKey {
		t.Errorf("banner: %v", c)
	}
	if c, _ := ClassifyLoose(255, "warning: x\n"+wlRefused+"\n"); c != ClassConnect {
		t.Errorf("connect among others: %v", c)
	}
	if c, _ := ClassifyLoose(1, wlRefused); c != ClassOther {
		t.Errorf("exit 1: %v", c)
	}
	if c, _ := ClassifyLoose(255, "client_loop: send disconnect: Broken pipe"); c != ClassOther {
		t.Errorf("other: %v", c)
	}
}
