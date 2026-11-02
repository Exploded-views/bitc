GO      ?= go
BIN     := dist/bitc
PKG     := github.com/Exploded-views/bitc
LDFLAGS := -s -w -X $(PKG)/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev) \
           -X $(PKG)/internal/version.Commit=$(shell git rev-parse --short=12 HEAD 2>/dev/null)

.PHONY: build test test-unit coverage-merge conformance-shard conformance-race-shard docs coverage coverage-ratchet conformance conformance-race lint darwin-vet vuln ci tools tools-ci snapshot release-check examples fuzz privileged secrets linkcheck

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/bitc

# The one unit-test run: its output is kept in $(COVER_OUT) for `make coverage`, so ci does not run the
# tests a second time.
COVER_OUT := dist/test-cover.txt
test:
	@mkdir -p dist
	@$(GO) test -race -cover ./internal/... ./cmd/... >$(COVER_OUT) 2>&1; rc=$$?; cat $(COVER_OUT); exit $$rc

# The same unit run in two legs, one CI job each (ci.yml: unit): providers is the provider and remote
# packages (the slow ones), core is every other package under internal and cmd. Each leg writes
# dist/test-cover-$(LEG).txt; `make coverage-merge` puts the legs back together.
UNIT_LEGS ?= providers core
LEG_RE    := /internal/(provider|remote)(/|$$)
test-unit:
	@mkdir -p dist
	@case "$(LEG)" in \
	  providers) pk="./internal/provider/... ./internal/remote/..." ;; \
	  core) pk=$$($(GO) list ./internal/... ./cmd/... | grep -Ev '$(LEG_RE)') ;; \
	  *) echo "make test-unit: LEG must be providers or core, not '$(LEG)'" >&2; exit 2 ;; \
	esac; \
	out=dist/test-cover-$(LEG).txt; \
	$(GO) test -race -cover $$pk >$$out 2>&1; rc=$$?; cat $$out; exit $$rc

# Merges the legs' outputs in dist/ into $(COVER_OUT), refusing a leg that is missing, empty or failed
# (scripts/coverage-merge.sh), then runs the coverage floors on the merged run: a package missing from
# it is a failure there, so coverage is never checked per leg.
coverage-merge:
	sh scripts/coverage-merge_test.sh
	sh scripts/coverage-merge.sh $(COVER_OUT) dist $(UNIT_LEGS)
	$(MAKE) coverage

# Per-package statement coverage may not fall below coverage-floors.json (scripts/coverage-floors.sh).
# Reads the output of the last `make test`; the script's own self-check runs first.
coverage:
	sh scripts/coverage-floors_test.sh
	sh scripts/coverage-floors.sh check $(COVER_OUT)

# Raises the floors to the current coverage (rounded down to 0.1; never lowers one). Runs the tests.
# Add DROP=--drop-removed to also drop entries of packages that no longer exist.
coverage-ratchet: test
	sh scripts/coverage-floors.sh ratchet $(DROP) $(COVER_OUT)

conformance: build
	$(GO) test -race -timeout 20m ./conformance/... -bin=$(BIN)

# The same suite against a race-instrumented bitc (spec 11: -race everywhere). -race needs cgo
# and gcc, which the toolchain container (scripts/dev.sh) has; the host has neither. The race
# binary runs the whole suite several times slower, past go test's default 10m, so set -timeout.
conformance-race:
	CGO_ENABLED=1 $(GO) build -race -o dist/bitc-race ./cmd/bitc
	$(GO) test -race -timeout 30m ./conformance/... -bin=dist/bitc-race

# One shard of the conformance suite (scripts/conformance-shards.sh: package, -run and -skip). `make ci`
# runs the whole suite unsharded; CI runs the shards in parallel, and the shard check in make lint
# fails when a test belongs to none or to two.
SHARD ?=
conformance-shard: build
	@set -e; cs=scripts/conformance-shards.sh; pkg=$$(sh $$cs pkg "$(SHARD)"); run=$$(sh $$cs run "$(SHARD)"); skip=$$(sh $$cs skip "$(SHARD)"); \
	$(GO) test -race -timeout 20m "$$pkg" -run "$$run" $${skip:+-skip "$$skip"} -bin=$(BIN)

conformance-race-shard:
	@set -e; cs=scripts/conformance-shards.sh; pkg=$$(sh $$cs pkg "$(SHARD)"); run=$$(sh $$cs run "$(SHARD)"); skip=$$(sh $$cs skip "$(SHARD)"); \
	CGO_ENABLED=1 $(GO) build -race -o dist/bitc-race ./cmd/bitc; \
	$(GO) test -race -timeout 30m "$$pkg" -run "$$run" $${skip:+-skip "$$skip"} -bin=dist/bitc-race

lint: darwin-vet
	$(GO) vet ./...
	golangci-lint run
	sh scripts/scrub_test.sh
	sh scripts/retry_test.sh
	sh scripts/coverage-merge_test.sh
	sh scripts/conformance-shards_test.sh

# The test code must compile for darwin: test code that needs Linux carries a linux build constraint
# or a _linux file name.
darwin-vet:
	GOOS=darwin $(GO) vet ./...

# Advisory (not in ci): maintainers run it on main from time to time; it blocks a release.
# Needs network access to vuln.go.dev.
vuln:
	govulncheck ./...

# Checks private 0600 copies: git records no mode but the exec bit, so a checkout under umask
# 002 leaves the examples 0664, which the loader refuses (spec 5.1).
examples: build
	@set -e; d=$$(mktemp -d); trap 'rm -rf "$$d"' EXIT; \
	for ex in config/host.bitc.toml,config/repo.bitc.toml node-monorepo/host.bitc.toml,node-monorepo/.bitc.toml python-monorepo/host.bitc.toml,python-monorepo/.bitc.toml; do \
	  install -m 0600 "examples/$${ex%,*}" "$$d/host.toml"; install -m 0600 "examples/$${ex#*,}" "$$d/repo.toml"; \
	  BITC_CONFIG="$$d/host.toml" ./$(BIN) config check --repo "$$d/repo.toml"; \
	done

# Every relative markdown link must resolve (scripts/linkcheck.sh).
linkcheck:
	sh scripts/linkcheck.sh

# What the docs CI job runs: the example configs load, every markdown link resolves.
docs: examples linkcheck

ci: lint test coverage conformance conformance-race examples linkcheck

# Every Fuzz* target for FUZZTIME each (not part of ci; ci's go test runs the seed corpora).
fuzz:
	@for pkg in $$($(GO) list ./internal/...); do \
	  for f in $$($(GO) test -list '^Fuzz' $$pkg | grep '^Fuzz'); do \
	    echo "fuzz $$pkg $$f"; \
	    $(GO) test -run '^$$' -fuzz "^$$f$$" -fuzztime $${FUZZTIME:-10s} $$pkg || exit 1; \
	  done; \
	done

# Whole-history secret scan (gitleaks; not part of `make ci`: the secrets job in ci.yml runs it on
# every push and pull request). Prints counts and rule/file/commit only, never a match.
# GITLEAKS_EXTRA is an optional extra config file.
secrets:
	sh scripts/secrets.sh

# Root-only agent suite in a throwaway container; run on the host (needs docker), not in ci.
privileged:
	sh scripts/privileged.sh

snapshot:
	goreleaser release --snapshot --clean

# Builds the release artifacts locally (snapshot: nothing is uploaded) and verifies them: eight
# assets, a tarball and a bare binary for linux and darwin on amd64 and arm64, all in checksums.txt.
release-check:
	goreleaser check
	goreleaser release --snapshot --clean
	@# a tarball sits in dist/; a bare binary bitc_<ver>_<os>_<arch> is the build output dist/bitc_<os>_<arch>_*/bitc
	cd dist && n=0 && while read -r sum name; do \
	  case "$$name" in \
	    *.tar.gz) f="$$name" ;; \
	    *) osarch=$$(echo "$$name" | sed 's/^bitc_[^_]*_//'); f=$$(ls bitc_$${osarch}_*/bitc 2>/dev/null | head -n 1) ;; \
	  esac; \
	  test -f "$$f" || { echo "release-check: no file for $$name"; exit 1; }; \
	  echo "$$sum  $$f" | sha256sum -c - || exit 1; \
	  n=$$((n + 1)); \
	done < checksums.txt && test "$$n" -eq 8
	tar -tzf dist/bitc_*_linux_amd64.tar.gz | grep -qx bitc
	tar -tzf dist/bitc_*_linux_arm64.tar.gz | grep -qx bitc
	tar -tzf dist/bitc_*_darwin_amd64.tar.gz | grep -qx bitc
	tar -tzf dist/bitc_*_darwin_arm64.tar.gz | grep -qx bitc
	test -x dist/bitc_linux_amd64_*/bitc
	test -x dist/bitc_linux_arm64_*/bitc
	test -x dist/bitc_darwin_amd64_*/bitc
	test -x dist/bitc_darwin_arm64_*/bitc

# Pinned exactly; a bump is a deliberate maintainer commit, never part of an unrelated change.
tools-ci:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
	$(GO) install github.com/goreleaser/goreleaser/v2@v2.18.3

tools: tools-ci
	$(GO) install golang.org/x/vuln/cmd/govulncheck@v1.8.0
	$(GO) install github.com/zricethezav/gitleaks/v8@v8.30.1

# Optional, not part of ci or the release builds: the Sprites probe tool (a live probe), built with its
# tag for linux/amd64 into dist/. Only the maintainers run it live.
.PHONY: probe-build
probe-build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -tags liveprobe -trimpath -o dist/bitc-sprites-probe ./cmd/bitc-sprites-probe
