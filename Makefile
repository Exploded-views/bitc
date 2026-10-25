GO      ?= go
BIN     := dist/bitc
PKG     := github.com/Exploded-views/bitc
LDFLAGS := -s -w -X $(PKG)/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev) \
           -X $(PKG)/internal/version.Commit=$(shell git rev-parse --short=12 HEAD 2>/dev/null)

.PHONY: build test coverage coverage-ratchet conformance conformance-race lint vuln ci tools tools-ci snapshot release-check examples fuzz privileged secrets linkcheck

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/bitc

# The one unit-test run: its output is kept in $(COVER_OUT) for `make coverage`, so ci does not run the
# tests a second time.
COVER_OUT := dist/test-cover.txt
test:
	@mkdir -p dist
	@$(GO) test -race -cover ./internal/... ./cmd/... >$(COVER_OUT) 2>&1; rc=$$?; cat $(COVER_OUT); exit $$rc

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

lint:
	$(GO) vet ./...
	golangci-lint run

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

ci: lint test coverage conformance conformance-race examples linkcheck

# Every Fuzz* target for FUZZTIME each (not part of ci; ci's go test runs the seed corpora).
fuzz:
	@for pkg in $$($(GO) list ./internal/...); do \
	  for f in $$($(GO) test -list '^Fuzz' $$pkg | grep '^Fuzz'); do \
	    echo "fuzz $$pkg $$f"; \
	    $(GO) test -run '^$$' -fuzz "^$$f$$" -fuzztime $${FUZZTIME:-10s} $$pkg || exit 1; \
	  done; \
	done

# Whole-history secret scan (gitleaks; a maintainer step, not part of ci). Prints counts and
# rule/file/commit only, never a match. GITLEAKS_EXTRA is an optional extra config file.
secrets:
	sh scripts/secrets.sh

# Root-only agent suite in a throwaway container; run on the host (needs docker), not in ci.
privileged:
	sh scripts/privileged.sh

snapshot:
	goreleaser release --snapshot --clean

# Builds the release artifacts locally (snapshot: nothing is uploaded) and verifies them.
release-check:
	goreleaser check
	goreleaser release --snapshot --clean
	@# a tarball sits in dist/; a bare binary is the build output dist/bitc_linux_<arch>_*/bitc
	cd dist && n=0 && while read -r sum name; do \
	  case "$$name" in \
	    *.tar.gz) f="$$name" ;; \
	    *) f=$$(ls bitc_linux_$${name##*_}_*/bitc 2>/dev/null | head -n 1) ;; \
	  esac; \
	  test -f "$$f" || { echo "release-check: no file for $$name"; exit 1; }; \
	  echo "$$sum  $$f" | sha256sum -c - || exit 1; \
	  n=$$((n + 1)); \
	done < checksums.txt && test "$$n" -eq 4
	tar -tzf dist/bitc_*_linux_amd64.tar.gz | grep -qx bitc
	tar -tzf dist/bitc_*_linux_arm64.tar.gz | grep -qx bitc
	test -x dist/bitc_linux_amd64_*/bitc
	test -x dist/bitc_linux_arm64_*/bitc

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
