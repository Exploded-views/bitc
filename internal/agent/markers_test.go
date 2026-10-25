package agent

import "testing"

func TestMarkers(t *testing.T) {
	m := "aa11"
	if line := FormatMarker(MarkExit, m, "3"); line != "__BITC_EXIT__ aa11 3" {
		t.Fatal(line)
	}
	if mk, ok := ParseMarker("__BITC_EXIT__ aa11 3", m); !ok || mk.Kind != MarkExit || mk.Arg != "3" {
		t.Fatal(mk, ok)
	}
	for _, line := range []string{"__BITC_EXIT__ bb22 3", "echo __BITC_EXIT__ aa11 3", "__BITC_EXIT__ aa11", "__BITC_EXIT__ aa11 x", "__BITC_NOPE__ aa11"} {
		if _, ok := ParseMarker(line, m); ok {
			t.Errorf("%q parsed as a marker", line)
		}
	}
	if mk, ok := ParseMarker("__BITC_SETUP_FAILED__ aa11 node", m); !ok || mk.Arg != "node" || mk.Class != "" {
		t.Fatal(mk) // no class: the host treats it as infra (75)
	}
	if line := FormatMarker(MarkSetupFailed, m, "deps", "repo", "npm ERR! missing script: ci"); line != "__BITC_SETUP_FAILED__ aa11 deps repo npm ERR! missing script: ci" {
		t.Fatal(line)
	}
	mk, ok := ParseMarker("__BITC_SETUP_FAILED__ aa11 deps repo npm ERR! missing script: ci", m)
	if !ok || mk.Arg != "deps" || mk.Class != "repo" || mk.Line != "npm ERR! missing script: ci" {
		t.Fatal(mk)
	}
	if mk, ok := ParseMarker("__BITC_SETUP_FAILED__ aa11 t infra HTTP 404", m); !ok || mk.Class != "infra" || mk.Line != "HTTP 404" {
		t.Fatal(mk)
	}
	if mk, ok := ParseMarker("__BITC_SETUP_FAILED__ aa11 deps REPO x", m); !ok || mk.Class != "" {
		t.Fatal(mk) // an unparsable class is not repo
	}
}
