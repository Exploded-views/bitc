package remote

import (
	"fmt"
	"strings"

	"github.com/Exploded-views/bitc/internal/config"
)

// quoteList renders a list as TOML, ["amd64", "arm64"], for notes.
func quoteList(l []string) string {
	q := make([]string, len(l))
	for i, s := range l {
		q[i] = fmt.Sprintf("%q", s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// SelectArch picks the architecture a job runs on: the first entry of the pool's arch list that
// the provider can run (providers delta C.4 item 1). A provider that lists none runs amd64 only.
// With no overlap the job is ineligible: `arch <provider's> not allowed by pool (arch = [...])`.
func SelectArch(providerArch, poolArch []string) (string, error) {
	if len(providerArch) == 0 {
		providerArch = []string{"amd64"}
	}
	if len(poolArch) == 0 {
		poolArch = []string{"amd64"}
	}
	for _, want := range poolArch {
		for _, have := range providerArch {
			if want == have {
				return have, nil
			}
		}
	}
	return "", ineligible("arch %s not allowed by pool (arch = %s)", providerArch[0], quoteList(poolArch))
}

// ToolsForArch keeps the tool entries for arch. A tool name with no entry for arch makes the job
// ineligible: `tool node has no arm64 entry` (providers delta C.9 item 3).
func ToolsForArch(tools []config.Tool, arch string) ([]config.Tool, error) {
	var out []config.Tool
	have := map[string]bool{}
	for _, t := range tools {
		if t.Arch == arch || (t.Arch == "" && arch == "amd64") {
			out = append(out, t)
			have[t.Name] = true
		}
	}
	for _, t := range tools {
		if !have[t.Name] {
			return nil, ineligible("tool %s has no %s entry", t.Name, arch)
		}
	}
	return out, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
