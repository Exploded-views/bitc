package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

var docBlock = regexp.MustCompile("(?s)<!-- doc-test -->\\s*```sh\\n(.*?)```")

func readDoc(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runDocBlocks(t *testing.T, file string) {
	t.Helper()
	h := harness.New(t)
	bin := filepath.Join(h.Root, "bin")
	_ = os.MkdirAll(bin, 0o755)
	_ = os.Symlink(harness.Bin, filepath.Join(bin, "bitc"))
	blocks := docBlock.FindAllStringSubmatch(readDoc(t, file), -1)
	if len(blocks) == 0 {
		t.Fatalf("%s has no doc-test blocks", file)
	}
	for i, b := range blocks {
		c := exec.Command("sh", "-e", "-c", b[1])
		c.Dir = h.Root
		c.Env = harness.MergeEnv(h.Env, []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "!BITC_CONFIG", "!XDG_CONFIG_HOME"})
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s block %d failed: %v\n%s\n--- block ---\n%s", file, i+1, err, out, b[1])
		}
	}
}

func TestDOC1_QuickStartRuns(t *testing.T) { runDocBlocks(t, "../README.md") }
func TestDOC2_AgentGuideRuns(t *testing.T) { runDocBlocks(t, "../docs/agent-guide.md") }

func TestDOC3_ExitCodesTableMatchesTheContract(t *testing.T) {
	doc := readDoc(t, "../docs/exit-codes.md")
	for _, code := range []string{"| 64 ", "| 66 ", "| 69 ", "| 75 ", "| 78 ", "| 124 "} {
		if !strings.Contains(doc, code) {
			t.Errorf("exit-codes.md lacks %s", strings.TrimSpace(code))
		}
	}
}
