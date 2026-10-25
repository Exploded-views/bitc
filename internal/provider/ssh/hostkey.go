package ssh

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// wireString reads one SSH wire-format string from b.
func wireString(b []byte) (s, rest []byte, ok bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, false
	}
	return b[4 : 4+n], b[4+n:], true
}

// ParseHostKey checks the host_key of the host file: an authorized_keys-style line (the type, the
// base64 blob, an optional comment). It returns the key type, or the reason it is not usable
// (providers delta C.10): ssh-dss and ssh-rsa under 3072 bits are refused.
func ParseHostKey(s string) (typ string, err error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return "", errors.New("required")
	}
	typ = f[0]
	if typ == "ssh-dss" {
		return "", errors.New("ssh-dss is not accepted")
	}
	switch typ {
	case "ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "ssh-rsa":
	default:
		return "", errors.New("unparsable")
	}
	if len(f) < 2 {
		return "", errors.New("unparsable")
	}
	blob, e := base64.StdEncoding.DecodeString(f[1])
	if e != nil {
		return "", errors.New("unparsable")
	}
	name, rest, ok := wireString(blob)
	if !ok || string(name) != typ {
		return "", errors.New("unparsable")
	}
	switch typ {
	case "ssh-ed25519":
		k, rest, ok := wireString(rest)
		if !ok || len(k) != 32 || len(rest) != 0 {
			return "", errors.New("unparsable")
		}
	case "ssh-rsa":
		_, rest, ok := wireString(rest) // e
		if !ok {
			return "", errors.New("unparsable")
		}
		n, rest, ok := wireString(rest)
		if !ok || len(rest) != 0 {
			return "", errors.New("unparsable")
		}
		if bits := new(big.Int).SetBytes(n).BitLen(); bits < 3072 {
			return "", fmt.Errorf("ssh-rsa key is %d bits, 3072 or more required", bits)
		}
	default: // ecdsa: curve name, then the point
		if _, rest, ok := wireString(rest); !ok {
			return "", errors.New("unparsable")
		} else if pt, rest, ok := wireString(rest); !ok || len(pt) == 0 || len(rest) != 0 {
			return "", errors.New("unparsable")
		}
	}
	return typ, nil
}

// KnownHostsLine is the one line of the pinned known_hosts file: `bitc-ssh <type> <key>`. The
// alias makes it independent of the host and port (providers delta C.2.2).
func KnownHostsLine(hostKey string) string {
	f := strings.Fields(hostKey)
	if len(f) < 2 {
		return ""
	}
	return "bitc-ssh " + f[0] + " " + f[1] + "\n"
}
