// Package classifytest holds the classifier fixtures shared by unit and conformance tests.
package classifytest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
)

// NodeMonorepoRules mirrors examples/node-monorepo/.bitc.toml [pools.lint] refuse.
func NodeMonorepoRules() []config.RefuseRule {
	return []config.RefuseRule{
		{Tool: "eslint", When: "repo-wide",
			UnlessArgs: []string{"--stdin", "--version", "-v", "--help", "-h", "--print-config", "--init", "--inspect-config", "--env-info", "--debug-info", "--mcp"},
			SkipOptionValues: []string{"-c", "--config", "--max-warnings", "--fix-type", "--ignore-path", "--suppress-rule", "--format", "-f", "--stdin-filename", "--rule", "--parser", "--plugin", "--ext", "--cache-location", "--rulesdir", "-o", "--output-file", "--resolve-plugins-relative-to",
				"--parser-options", "--ignore-pattern", "--env", "--global", "--cache-strategy", "--report-unused-disable-directives-severity", "--suppressions-location", "--concurrency", "--flag"},
			Reason: "lint only changed files here; repo-wide lint runs in the heavy pool"},
		{Tool: "tsc", When: "always", UnlessArgs: []string{"-v", "--version", "-h", "--help", "--init", "--showConfig"},
			Reason: "typecheck runs in the heavy pool"},
		{NpmScript: []string{"lint", "lint:fix", "lint:ci", "typecheck", "check:types", "test:types"},
			Reason: "repo-wide lint and typecheck run in the heavy pool"},
	}
}

// RefusedFromRoot: every row must be refused with cwd = work-tree root.
var RefusedFromRoot = joinRows(refusedFromRootRows, refusedMoreRows, eslintValueRows(), wrapperValueRows(), nodeValueRows())

// joinRows concatenates argv tables.
func joinRows(tables ...[][]string) [][]string {
	var out [][]string
	for _, t := range tables {
		out = append(out, t...)
	}
	return out
}

var refusedFromRootRows = [][]string{
	{"eslint", "."}, {"npx", "eslint", "."}, {"npx", "eslint", "./"},
	{"npx", "--no-install", "eslint", "--max-warnings=0", "."}, {"npx", "eslint", "--max-warnings", "0", "."},
	{"npx", "eslint", "-c", "eslint.config.mjs", "."}, {"node_modules/.bin/eslint", "--fix", "."},
	{"npx", "eslint"}, {"npx", "eslint", "--max-warnings=0"},
	{"npm", "run", "lint"}, {"npm", "run", "lint:fix"}, {"npm", "run", "lint:ci"}, {"npm", "run", "typecheck"},
	{"npm", "run", "check:types"}, {"npm", "run", "test:types"}, {"npm", "run", "-s", "test:types"},
	{"pnpm", "test:types"}, {"pnpm", "run", "test:types"}, {"yarn", "test:types"},
	{"bash", "-c", "npm run test:types"}, {"npx", "tsc", "--noEmit", "-p", "e2e/tsconfig.json"},
	{"npm", "run", "lint", "--", "--quiet"}, {"npm", "run-script", "lint"}, {"npx", "tsc", "--noEmit"},
	{"tsc", "-p", "tsconfig.app.json"}, {"env", "LINT_FAST=1", "npx", "eslint", "."},
	{"bash", "-c", "npm run lint"}, {"bash", "-c", "cd src && npx eslint . --fix"},
	{"timeout", "600", "npx", "eslint", "."}, {"timeout", "-k", "5", "600", "npx", "eslint", "."},
	{"nice", "-n", "10", "npx", "eslint", "."}, {"env", "-u", "X", "npx", "eslint", "."},
	{"env", "-u", "X", "-u", "Y", "A=1", "npx", "eslint", "."}, {"time", "-p", "npx", "eslint", "."},
	{"nohup", "npx", "eslint", "."}, {"env", "nice", "-n", "5", "timeout", "60", "npx", "eslint"},
	{"bash", "-c", "LINT_FAST=1 npx eslint ."}, {"bash", "-c", "timeout 600 npm run lint"},
	{"bash", "-lc", "npm run lint"}, {"bash", "-ec", "npx tsc --noEmit"}, {"sh", "-c", "if true; then npx eslint .; fi"},
	{"bash", "-c", "echo start; (npx eslint .)"}, {"bash", "-c", "true || npx eslint ."},
	{"bash", "-c", "nice -n 10 env -u X npx eslint ."}, {"bash", "-c", `npx eslint "$PWD"`},
	{"bash", "-c", "bash -c 'npm run typecheck'"},
	{"npm", "run", "-s", "lint"}, {"npm", "--silent", "run", "lint"}, {"npm", "--prefix", ".", "run", "lint"},
	{"npm", "run", "--if-present", "check:types"}, {"npm", "exec", "eslint", "."}, {"npm", "exec", "--", "eslint", "."},
	{"npm", "x", "tsc"}, {"pnpm", "lint"}, {"pnpm", "run", "lint"}, {"pnpm", "exec", "tsc"}, {"yarn", "lint"},
	{"yarn", "typecheck"}, {"npx", "-p", "typescript", "tsc"}, {"npx", "-c", "eslint ."},
	{"npx", "eslint", "./."}, {"npx", "eslint", "src"}, {"npx", "eslint", "src/"},
	{"npx", "eslint", "--max-warnings=0", "src/a.ts", "src"}, {"npx", "eslint", "$PWD"}, {"npx", "eslint", "${PWD}"},
	{"npx", "eslint", ".."}, {"npx", "eslint", "/"}, {"npx", "eslint", "/."},
	{"npx", "eslint", "--max-warnings=0", "src/a.ts", "/"},
	{"bash", "-c", "npx eslint --max-warnings=0 --pass-on-unpruned-suppressions > lint.log 2>&1"},
	{"bash", "-c", "npx eslint --max-warnings=0 >> lint.log"}, {"bash", "-c", "npx eslint --fix 2> lint.err"},
	{"bash", "-c", "npx eslint &> lint.log"}, {"bash", "-c", "npx eslint >lint.log 2>&1"},
	{"bash", "-c", "npx eslint < /dev/null > lint.log"}, {"bash", "-c", "npx eslint 2>&1"},
	{"bash", "-c", "npx eslint --max-warnings=0 --pass-on-unpruned-suppressions 2>&1"},
	{"bash", "-c", "npx eslint >&2"}, {"bash", "-c", "npx eslint &>lint.log"}, {"bash", "-c", "npx eslint&>lint.log"},
	{"bash", "-c", "npx eslint &>> lint.log"}, {"bash", "-c", "npx eslint <&0"}, {"bash", "-c", "npx eslint >| lint.log"},
	{"bash", "-c", "npx eslint --fix >lint.log 2>&1"}, {"bash", "-c", "npx eslint .>lint.log"},
	{"bash", "-c", "npx eslint .>>lint.log"}, {"bash", "-c", "npx eslint .>lint.log 2>&1"},
	{"bash", "-c", "npx eslint ./>lint.log"}, {"bash", "-c", "npx eslint src>lint.log"},
	{"bash", "-c", "npx eslint --max-warnings=0>lint.log"}, {"bash", "-c", "npx eslint .</dev/null"},
	{"npx", "eslint@9", "."}, {"npx", "--yes", "eslint@9.30.0", "--max-warnings=0", "."}, {"npx", "eslint@9"},
	{"npx", "tsc@5", "--noEmit"}, {"node", "node_modules/eslint/bin/eslint.js", "."},
	{"node", "--max-old-space-size=3072", "node_modules/eslint/bin/eslint.js"},
	{"node", "-r", "dotenv/config", "node_modules/eslint/bin/eslint.js", "."},
	{"node", "node_modules/typescript/bin/tsc", "--noEmit"}, {"bash", "-c", "node node_modules/eslint/bin/eslint.js ."},
	{"npx", "eslint", "--fix", "--fix-type", "problem"}, {"npx", "eslint", "--fix-type", "problem,suggestion", "--fix"},
	{"npx", "eslint", "--ignore-path", ".gitignore"}, {"npx", "eslint", "--suppress-rule", "no-console"},
	{"bash", "-c", "npx eslint $(pwd)"}, {"bash", "-c", "npx eslint `pwd`"},
	// bitc additions: shell options with values before -c, and env -S
	{"bash", "-o", "pipefail", "-c", "npx eslint ."}, {"bash", "+O", "extglob", "-ec", "npm run lint"},
	{"env", "-S", "npx eslint ."}, {"env", "--split-string=npx tsc --noEmit"},
}

// AllowedFromRoot: every row must pass with cwd = work-tree root.
var AllowedFromRoot = joinRows(allowedFromRootRows, allowedMoreRows, infoFlagRows())

var allowedFromRootRows = [][]string{
	{"npx", "eslint", "--max-warnings=0", "src/a.ts", "src/b.ts"}, {"npx", "eslint", "--max-warnings", "0", "--format", "json", "src/a.ts"},
	{"eslint", "--fix", "-c", "eslint.config.mjs", "scripts/check.sh"}, {"npx", "eslint", "src/.hidden.ts"},
	{"npx", "eslint", "--stdin", "--stdin-filename", "src/a.ts"}, {"npx", "eslint", "--version"},
	{"npx", "prettier", "--check", "."}, {"bash", "-c", "echo eslint"}, {"git", "status"},
	{"timeout", "600", "npx", "eslint", "src/a.ts"}, {"nice", "-n", "10", "npx", "eslint", "--max-warnings=0", "src/a.ts"},
	{"env", "-u", "X", "npx", "eslint", "src/a.ts"}, {"env", "A=1", "B=2", "npx", "eslint", "src/a.ts"},
	{"bash", "-c", "LINT_FAST=1 npx eslint src/a.ts"}, {"bash", "-lc", "npx eslint src/a.ts src/b.ts"},
	{"bash", "-c", "timeout 600 npx eslint src/a.ts && echo done"}, {"npm", "run", "-s", "test"},
	{"npm", "--silent", "run", "build"}, {"npm", "exec", "eslint", "src/a.ts"}, {"pnpm", "exec", "eslint", "src/a.ts"},
	{"yarn", "test"}, {"npx", "-p", "typescript", "tsc", "--version"}, {"npx", "eslint", "src/a.ts", "tests/a.ts"},
	{"command", "-v", "eslint"}, {"timeout", "5", "git", "status"},
	{"bash", "-c", "npx eslint --max-warnings=0 src/a.ts > lint.log 2>&1"}, {"bash", "-c", "npx eslint src/a.ts >> lint.log"},
	{"bash", "-c", "npx eslint src/a.ts 2>/dev/null"}, {"bash", "-c", "npx eslint src/a.ts &> lint.log"},
	{"bash", "-c", "npx eslint --fix src/a.ts src/b.ts >lint.log 2>&1 && echo done"},
	{"bash", "-c", "npx eslint --max-warnings=0 2>&1 src/a.ts"}, {"bash", "-c", "npx eslint 2>&1 src/a.ts src/b.ts"},
	{"bash", "-c", "npx eslint >&2 src/a.ts"}, {"bash", "-c", "npx eslint &>lint.log src/a.ts"},
	{"bash", "-c", "npx eslint &>> lint.log src/a.ts"}, {"bash", "-c", "npx eslint <&0 src/a.ts"},
	{"bash", "-c", "npx eslint >| lint.log src/a.ts"}, {"bash", "-c", "npx eslint 2>/dev/null src/a.ts"},
	{"bash", "-c", "npx eslint >lint.log src/a.ts"}, {"bash", "-c", "npx eslint src/a.ts>lint.log"},
	{"bash", "-c", "npx eslint src/a.ts>>lint.log 2>&1"}, {"bash", "-c", "npx eslint --fix src/a.ts src/b.ts>lint.log"},
	{"bash", "-c", "npx eslint src/a.ts</dev/null"},
	{"npx", "eslint", "src/app/api/chat"}, {"npx", "eslint", "src/components/editor", "--max-warnings=0"},
	{"npx", "eslint", "src/app/(auth)/login"}, {"npx", "eslint", "e2e/targets"},
	{"npx", "eslint", "e2e/tests/T11", "e2e/schemas/T11"}, {"npx", "eslint", "./e2e/scripts/"},
	{"npx", "eslint@9", "src/app/api/chat"}, {"npx", "eslint", "--fix", "--fix-type", "problem", "src/a.ts"},
	{"npx", "eslint", "--ignore-path", ".gitignore", "src/a.ts"}, {"node", "node_modules/eslint/bin/eslint.js", "src/a.ts"},
	{"node", "--version"}, {"node", "script.js", "."}, {"npx", "eslint@9", "--version"},
	{"bash", "-c", `npx eslint $(git diff --name-only HEAD -- "*.ts")`},
	{"bash", "-o", "pipefail", "-c", "npx eslint src/a.ts"}, {"env", "-S", "npx eslint", "src/a.ts"},
}

// EslintValueOptions are the eslint options that take a value and are listed in
// skip_option_values: `npx eslint <opt> x` names no file, so it is a repo-wide lint, while a
// file after the value is scoped.
var EslintValueOptions = []string{"-c", "--config", "--max-warnings", "--fix-type", "--ignore-path", "--suppress-rule",
	"--format", "-f", "--stdin-filename", "--rule", "--parser", "--plugin", "--ext", "--cache-location", "--rulesdir", "-o",
	"--output-file", "--resolve-plugins-relative-to", "--parser-options", "--ignore-pattern", "--env", "--global",
	"--cache-strategy", "--report-unused-disable-directives-severity", "--suppressions-location", "--concurrency", "--flag"}

// EslintInfoFlags lint nothing (information and setup flags); eslint and tsc with them are allowed.
var EslintInfoFlags = []string{"--stdin", "--version", "-v", "--help", "-h", "--print-config", "--init", "--inspect-config",
	"--env-info", "--debug-info", "--mcp"}

// refusedMoreRows are the wrapper, shell, runner, redirection and keyword spellings that must
// be seen through (spec 6.7).
var refusedMoreRows = [][]string{
	// eslint without a file after a value option, and the plain wrapper spellings
	{"npx", "eslint", "--max-warnings", "0"}, {"npx", "eslint", "-c", "eslint.config.mjs"},
	{"npx", "eslint", "--config", "eslint.config.mjs"}, {"bash", "-c", "npx eslint --max-warnings 0"},
	{"ionice", "npx", "eslint", "."}, {"ionice", "-c", "3", "npx", "eslint", "."},
	{"stdbuf", "-oL", "npx", "eslint", "."}, {"stdbuf", "-o", "L", "npx", "eslint", "."},
	{"exec", "npx", "eslint", "."}, {"exec", "-a", "lint", "npx", "eslint", "."},
	{"command", "npx", "eslint", "."}, {"command", "-p", "npx", "eslint", "."}, {"setsid", "npx", "eslint", "."},
	{"env", "-C", "src", "npx", "eslint", "."}, {"nice", "--adjustment", "10", "npx", "eslint", "."},
	{"time", "-f", "%e", "npx", "eslint", "."}, {"time", "-o", "time.log", "npx", "eslint", "."},
	{"timeout", "-s", "KILL", "600", "npx", "eslint", "."},
	{"bash", "-c", "ionice -c 3 npm run lint"}, {"bash", "-c", "stdbuf -oL npx eslint ."}, {"bash", "-c", "exec npx eslint ."},
	{"bash", "-c", "command npx eslint ."}, {"bash", "-c", "setsid npx eslint ."},
	// env -i keeps the command visible (a lone "-" after env is documented as a hole, not pinned)
	{"env", "-i", "npx", "eslint", "."},
	// other shells, and shell options written as words before -c
	{"zsh", "-c", "npx eslint ."}, {"dash", "-c", "npx eslint ."}, {"ksh", "-c", "npx eslint ."},
	{"zsh", "-c", "npm run lint"}, {"dash", "-c", "npm run typecheck"}, {"ksh", "-c", "npx tsc --noEmit"},
	{"bash", "+o", "pipefail", "-c", "npx eslint ."}, {"bash", "--rcfile", "rc", "-c", "npx eslint ."},
	{"bash", "--init-file", "rc", "-c", "npx eslint ."}, {"bash", "-e", "-c", "npx eslint ."},
	{"bash", "--login", "-c", "npm run lint"}, {"sh", "-x", "-c", "npx eslint ."},
	{"bash", "-e", "-x", "-c", "npx tsc --noEmit"}, {"bash", "+e", "-c", "npx eslint ."},
	// package runners
	{"pnpx", "eslint", "."}, {"bunx", "eslint", "."}, {"pnpx", "tsc", "--noEmit"}, {"bunx", "tsc"},
	{"nodejs", "node_modules/eslint/bin/eslint.js", "."}, {"npm", "rum", "lint"}, {"npm", "urn", "lint"},
	{"npm", "rum", "typecheck"}, {"pnpm", "dlx", "eslint", "."}, {"pnpm", "dlx", "tsc"}, {"yarn", "dlx", "eslint", "."},
	{"npx", "--call", "eslint ."}, {"npx", "--call", "npm run lint"},
	{"npx", "--package", "typescript", "tsc", "--noEmit"},
	{"npx", "-w", "web", "tsc", "--noEmit"}, {"npx", "--workspace", "web", "tsc", "--noEmit"},
	{"npx", "--prefix", "web", "tsc", "--noEmit"}, {"npx", "-w", "web", "eslint"},
	{"npx", "--workspace", "web", "eslint"}, {"npx", "--prefix", "web", "eslint"},
	// npm, pnpm and yarn global options and run options that take a value
	{"npm", "--prefix", "web", "run", "lint"}, {"npm", "-C", "web", "run", "lint"}, {"pnpm", "--dir", "web", "lint"},
	{"pnpm", "--dir", "web", "run", "lint"}, {"yarn", "--cwd", "web", "lint"}, {"npm", "-w", "web", "run", "lint"},
	{"npm", "--workspace", "web", "run", "lint"}, {"pnpm", "--filter", "web", "run", "lint"}, {"pnpm", "-F", "web", "lint"},
	{"npm", "--loglevel", "error", "run", "lint"}, {"npm", "--cache", "c", "run", "lint"},
	{"npm", "--registry", "r", "run", "lint"}, {"npm", "--userconfig", "u", "run", "lint"},
	{"npm", "--globalconfig", "g", "run", "lint"}, {"npm", "run", "--prefix", "web", "lint"},
	{"npm", "run", "-w", "web", "lint"}, {"npm", "run", "--workspace", "web", "lint"},
	{"pnpm", "run", "--filter", "web", "lint"}, {"pnpm", "run", "-F", "web", "lint"},
	{"pnpm", "run", "--dir", "web", "lint"}, {"npm", "run", "-C", "web", "lint"},
	// node: other bin script spellings
	{"node", "node_modules/eslint/bin/eslint.cjs", "."}, {"node", "node_modules/eslint/bin/eslint.mjs", "."},
	{"node", "node_modules/typescript/lib/tsc.js", "--noEmit"}, {"node", "node_modules/.bin/tsc", "--noEmit"},
	{"node", "node_modules/.bin/eslint", "."}, {"node", "node_modules/.bin/eslint"},
	// shell keywords before a command, each alone
	{"bash", "-c", "if false; then :; else npx eslint .; fi"}, {"bash", "-c", "if false; then :; elif npx eslint .; then :; fi"},
	{"bash", "-c", "for f in a; do npx eslint .; done"}, {"bash", "-c", "while npx eslint .; do break; done"},
	{"bash", "-c", "until npx eslint .; do :; done"}, {"bash", "-c", "{ npx eslint .; }"}, {"bash", "-c", "! npx eslint ."},
	{"bash", "-c", "if npx eslint .; then :; fi"}, {"bash", "-c", "if true; then npm run lint; fi"},
	{"bash", "-c", "for d in a; do npm run typecheck; done"}, {"bash", "-c", "if true; then eslint .; fi"},
	{"bash", "-c", "! npm run lint"},
	// a redirection glued to a word and followed by a space; two operators glued into one token
	{"bash", "-c", "npx eslint --fix> lint.log"}, {"bash", "-c", "npx eslint --max-warnings=0> lint.log 2>&1"},
	{"bash", "-c", "npx eslint .> lint.log"}, {"bash", "-c", "npx eslint --fix>> lint.log"},
	{"bash", "-c", "npx eslint --fix< /dev/null"}, {"bash", "-c", "npx eslint </dev/null>lint.log"},
	{"bash", "-c", "npx eslint --max-warnings=0 </dev/null>lint.log 2>&1"}, {"bash", "-c", "npx eslint >lint.log</dev/null"},
	// wrappers, shells and runners named by their path
	{"/usr/bin/time", "-v", "npx", "eslint", "."}, {"/usr/bin/env", "npx", "eslint", "."},
	{"/bin/env", "-u", "X", "npx", "eslint", "."}, {"/usr/bin/timeout", "600", "npm", "run", "lint"},
	{"/usr/bin/nohup", "npx", "eslint", "."}, {"/usr/bin/npx", "eslint", "."}, {"/usr/bin/bash", "-c", "npx eslint ."},
	{"bash", "-c", "/usr/bin/nice -n 5 npx eslint ."},
	// option clusters that carry -c with another letter, either side, either case
	{"bash", "-ce", "npx eslint ."}, {"bash", "-ec", "npx eslint ."}, {"bash", "-xec", "npx eslint ."},
	{"sh", "-cx", "npm run lint"}, {"bash", "-Ec", "npx eslint ."}, {"bash", "-cE", "npx eslint ."},
	// VAR=value prefixes of every shape
	{"bash", "-c", "npm_config_loglevel=silent npm run lint"}, {"bash", "-c", "E2E_LOCAL_WORKERS=1 npx eslint ."},
	{"bash", "-c", "_X=1 npx eslint ."}, {"bash", "-c", "a=1 npx eslint ."}, {"env", "e2e_x=1", "npx", "eslint", "."},
	{"LINT_FAST=1", "npm", "run", "lint"}, {"A=1", "B=2", "npm", "run", "lint"}, {"bash", "-c", "LINT_FAST=1 npm run lint"},
	{"env", "LINT_FAST=1", "npm", "run", "lint"}, {"bash", "-c", "NODE_HEAP_MB=4096 npm run typecheck"},
	{"bash", "-c", "A=1 B=2 npx tsc --noEmit"}, {"bash", "-c", "X79=1 npx eslint ."},
	// here-strings, glued and spaced
	{"bash", "-c", "npx eslint <<< x"}, {"bash", "-c", "npx eslint --fix<<< x"}, {"bash", "-c", "npx eslint <<<x"},
	// a file descriptor of more than one digit, and fds 6-9, is still a file descriptor
	{"bash", "-c", "npx eslint 10>lint.log"}, {"bash", "-c", "npx eslint 10> lint.log"},
	{"bash", "-c", "npx eslint --fix 10>lint.log"}, {"bash", "-c", "npx eslint 12>>lint.log"},
	{"bash", "-c", "npx eslint 10< /dev/null"}, {"bash", "-c", "npx eslint 7>lint.log"},
	{"bash", "-c", "npx eslint 9> lint.log"}, {"bash", "-c", "npx eslint 689>lint.log"},
	// the -- that ends the options of a wrapper, node or npm
	{"env", "--", "npx", "eslint", "."}, {"nice", "--", "npx", "eslint", "."},
	{"timeout", "--", "600", "npm", "run", "lint"}, {"npm", "--", "run", "lint"},
	{"node", "--", "node_modules/eslint/bin/eslint.js", "."}, {"bash", "-c", "env -- npx eslint ."},
	// one-word commands in strings, npx --, a nested sh -c
	{"bash", "-c", "eslint"}, {"bash", "-c", "tsc"}, {"bash", "-c", "echo start; eslint"}, {"npx", "eslint", "--"},
	{"bash", "-c", "sh -c 'npx eslint .'"},
	// node: -p / -e belong to node only; a node option is matched as a whole word
	{"node", "node_modules/typescript/bin/tsc", "-p", "tsconfig.app.json"},
	{"node", "--max-old-space-size=8192", "node_modules/typescript/bin/tsc", "-p", "tsconfig.app.json", "--noEmit"},
	{"bash", "-c", "node node_modules/.bin/tsc -p tsconfig.app.json"},
	{"node", "--inspect", "node_modules/eslint/bin/eslint.js", "."}, {"node", "-i", "node_modules/eslint/bin/eslint.js", "."},
	{"node", "-title", "node_modules/eslint/bin/eslint.js", "."},
	// the npm alias spelling of a package is still that package
	{"npx", "eslint@npm:eslint@9", "."},
}

// eslintValueRows: every eslint option that takes a value, with no file after it, is repo-wide.
func eslintValueRows() [][]string {
	var out [][]string
	for _, o := range EslintValueOptions {
		out = append(out, []string{"npx", "eslint", o, "x"})
	}
	return out
}

// wrapperValueRows: every wrapper option that takes a value, then the repo-wide command. env -S
// takes a string of words, so it is not a value option in this sense.
func wrapperValueRows() [][]string {
	var out [][]string
	for _, w := range []struct {
		name string
		opts []string
		pos  []string
	}{
		{"env", []string{"-u", "-C", "--unset", "--chdir"}, nil},
		{"nice", []string{"-n", "--adjustment"}, nil},
		{"time", []string{"-f", "-o", "--format", "--output"}, nil},
		{"timeout", []string{"-k", "-s", "--kill-after", "--signal"}, []string{"600"}},
		{"ionice", []string{"-c", "-n", "-p", "-P", "-u", "--class", "--classdata", "--pid", "--pgid", "--uid"}, nil},
		{"stdbuf", []string{"-i", "-o", "-e", "--input", "--output", "--error"}, nil},
		{"exec", []string{"-a"}, nil},
	} {
		for _, o := range w.opts {
			row := append([]string{w.name, o, "x"}, w.pos...)
			out = append(out, append(row, "npx", "eslint", "."))
		}
	}
	return out
}

// nodeValueRows: every node option that takes a value, then the eslint bin script.
func nodeValueRows() [][]string {
	var out [][]string
	for _, o := range []string{"-r", "--require", "--import", "--loader", "--experimental-loader", "--env-file",
		"--conditions", "-C", "--title", "--inspect-port"} {
		out = append(out, []string{"node", o, "x", "node_modules/eslint/bin/eslint.js", "."})
	}
	return out
}

// infoFlagRows: eslint information flags lint nothing.
func infoFlagRows() [][]string {
	var out [][]string
	for _, f := range EslintInfoFlags {
		out = append(out, []string{"npx", "eslint", f})
	}
	return out
}

// allowedMoreRows are commands that name a tool without linting files, or lint a file.
var allowedMoreRows = [][]string{
	{"bash", "-c", "npx eslint 10>lint.log src/a.ts"}, {"bash", "-c", "npx eslint 10> lint.log src/a.ts"},
	{"bash", "-c", "npx eslint src/a.ts> lint.log"}, {"bash", "-c", "npx eslint --max-warnings=0 src/a.ts> lint.log 2>&1"},
	{"bash", "-c", "npx eslint src/a.ts </dev/null>lint.log"},
	{"bash", "-ce", "npx eslint src/a.ts"}, {"bash", "-Ec", "npx eslint src/a.ts"},
	{"bash", "-c", "npm_config_loglevel=silent npx eslint src/a.ts"}, {"bash", "-c", "LINT_FAST=1 npx eslint src/a.ts"},
	{"bash", "-c", "npx eslint <<< x src/a.ts"},
	{"/usr/bin/env", "npx", "eslint", "src/a.ts"}, {"/usr/bin/timeout", "600", "npx", "eslint", "src/a.ts"},
	{"env", "--", "npx", "eslint", "src/a.ts"}, {"nice", "--", "npx", "eslint", "src/a.ts"},
	{"timeout", "--", "600", "npx", "eslint", "src/a.ts"}, {"node", "--", "node_modules/eslint/bin/eslint.js", "src/a.ts"},
	{"node", "node_modules/.bin/eslint", "src/a.ts"}, {"env", "LINT_FAST=1", "npm", "run", "test"},
	{"node", "--inspect", "node_modules/eslint/bin/eslint.js", "src/a.ts"}, {"node", "-i", "node_modules/eslint/bin/eslint.js", "src/a.ts"},
	{"bash", "-c", "if true; then npx eslint src/a.ts; fi"},
	// commands that name a tool without running it on files
	{"bash", "-c", "npx eslint src/a.ts", "_", "."}, {"bash", "-x"}, {"env", "A=1"}, {"nice"},
	{"command", "-V", "eslint"}, {"command", "-pv", "eslint"}, {"command", "-pV", "eslint"},
	{"npx", "--yes"}, {"npx", "--version"}, {"npm", "--silent"}, {"npm", "--version"},
	{"npx", "-p", "tsc"}, {"npx", "--package", "tsc"}, {"npx", "-c"}, {"npx", "eslint", "--", "-x.ts"},
	// node evaluating a string is not node running a tool's bin script
	{"node", "-e", "eslint", "."}, {"node", "-p", "eslint", "."}, {"node", "--eval", "eslint", "."},
	{"node", "--print", "eslint", "."}, {"node", "-pe", "eslint", "."}, {"node", "-ep", "eslint", "."},
	{"tsc", "--version"}, {"tsc", "--init"}, {"tsc", "--showConfig"}, {"npx", "tsc", "--version"},
	// a package script is matched as a whole word
	{"npm", "run", "e2e"}, {"npm", "run", "fix"}, {"npm", "run", "lin"}, {"npm", "run", "app"}, {"npm", "run"},
	// a shell with no string and a shell script file pass through
	{"bash", "-c"}, {"bash", "s.sh"},
	// options with a value, then a file
	{"npx", "eslint", "--max-warnings", "0", "src/a.ts"}, {"npx", "eslint", "-c", "eslint.config.mjs", "src/a.ts"},
	{"node", "--env-file", "x", "node_modules/eslint/bin/eslint.js", "src/a.ts"},
	{"node", "--inspect-port", "x", "node_modules/eslint/bin/eslint.js", "src/a.ts"},
	{"ionice", "-p", "1", "npx", "eslint", "src/a.ts"},
}

// From src/app of a work tree (with src/app/self -> .): eslint path arguments.
var RefusedFromSubdir = [][]string{
	{"."}, {"./"}, {".."}, {"../.."}, {"../../src"}, {"../../e2e"}, {"../../scripts"}, {"self"}, {"/"},
	{"../app/."}, {"components", "."}, {"$PWD"},
}
var AllowedFromSubdir = [][]string{
	{"components"}, {"./components/"}, {"api", "components"}, {"../../e2e/targets"}, {"../../src/app/api"}, {"self/components"},
}

// From src/lib/auth-utils: a sibling directory whose path is a string prefix of the cwd's path
// (src/lib/auth) is not an ancestor, so it is allowed; the real ancestors stay refused.
var AllowedFromPrefixSibling = [][]string{{"../auth"}, {"../auth/"}, {"../auth/login.ts"}}
var RefusedFromPrefixSibling = [][]string{{".."}, {"../.."}, {"."}}

// FixtureTree builds the work tree the tables assume and returns its canonical root.
func FixtureTree(t testing.TB) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".git", "src/app/components", "src/app/api/chat", "src/components/editor", "src/app/(auth)/login", "src/lib/auth", "src/lib/auth-utils",
		"e2e/targets", "e2e/tests/T11", "e2e/schemas/T11", "e2e/scripts", "scripts", "docs", "tests"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(".", filepath.Join(root, "src", "app", "self")); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestScriptRules refuses the npm script "test"; PackageManagerSpellings are every spelling
// of running it that must be refused (audit D).
func TestScriptRules() []config.RefuseRule {
	return []config.RefuseRule{{NpmScript: []string{"test"}, Reason: "tests run in the heavy pool"}}
}

var PackageManagerSpellings = [][]string{
	{"npm", "test"}, {"npm", "t"}, {"npm", "tst"}, {"npm", "run", "test"},
	{"npm", "run", "-w", "pkg", "test"}, {"npm", "run", "--workspace", "pkg", "test"},
	{"npm", "run", "--prefix", "sub", "test"}, {"npm", "--loglevel", "silent", "run", "test"},
	{"npm", "--cache", "/tmp/c", "run", "test"}, {"npm", "--registry", "http://r", "test"},
	{"npm", "--userconfig", "u", "run", "test"}, {"npm", "--globalconfig", "g", "run", "test"},
	{"npm", "-w", "pkg", "test"}, {"pnpm", "run", "--filter", "pkg", "test"},
	{"pnpm", "run", "-F", "pkg", "test"}, {"pnpm", "--filter", "pkg", "test"}, {"yarn", "run", "--cwd", "pkg", "test"},
}

// ToolSpellings are refused by NodeMonorepoRules from the work-tree root.
var ToolSpellings = [][]string{
	{"npm", "exec", "-w", "pkg", "eslint", "."}, {"npm", "exec", "--workspace", "pkg", "eslint", "."},
	{"npm", "exec", "--prefix", "sub", "eslint", "."}, {"npm", "--loglevel", "silent", "exec", "eslint", "."},
	{"pnpm", "--filter", "pkg", "exec", "tsc"}, {"npx", "-w", "pkg", "eslint", "."},
}
