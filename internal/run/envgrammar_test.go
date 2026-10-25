package run

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
)

func TestParseEnvOutput(t *testing.T) {
	in := "# comment\n\nexport A=plain\nB='lit $x `y`'\nC=\"q \\\"x\\\" \\\\ \\$HOME \\`\"\nD=con'cat'\"enated\"\n  \nE=\n"
	got, err := ParseEnvOutput([]byte(in))
	want := []KV{{"A", "plain"}, {"B", "lit $x `y`"}, {"C", "q \"x\" \\ $HOME `"}, {"D", "concatenated"}, {"E", ""}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%q %v", got, err)
	}
}

func TestParseEnvOutputRejects(t *testing.T) {
	for _, bad := range []string{
		"A=$HOME\n", "A=`id`\n", "A=x;rm\n", "A=x|y\n", "A=(x)\n", "A=x y\n", "1A=x\n", "A\n",
		"A=\"$x\"\n", "A=\"`x`\"\n", "A='unterminated\n", "A=\"unterminated\n", "A=x\r\n", "BITC_SLOT=9\n",
		"echo hi\n", "A=x\\y\n", "A='x\ry'\n", "A='x\x00y'\n", "A='x\x7fy'\n",
	} {
		if _, err := ParseEnvOutput([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// FuzzParseEnvOutput: any hook output parses or is rejected; a parsed value never carries a
// NUL or a CR (ruling 13) and every name is a plain environment name.
func FuzzParseEnvOutput(f *testing.F) {
	for _, s := range []string{"A=1\n", "export B='x y'\n", "C=\"q\\\"\"\n", "D=1\r\n", "# c\n\nE=", "F=$(x)\n", "G='unterminated\n", "=x\n", "H='x\ry'\n"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		kv, err := ParseEnvOutput(b)
		if err != nil {
			return
		}
		for _, x := range kv {
			if !config.ValidEnvName(x.Name) && !strings.HasPrefix(x.Name, "BITC_") {
				t.Fatalf("name %q from %q", x.Name, b)
			}
			if strings.ContainsAny(x.Value, "\x00\r") {
				t.Fatalf("value %q from %q", x.Value, b)
			}
		}
	})
}
