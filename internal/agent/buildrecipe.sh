# The reproducible agent build (spec 8.4 step 1, source = "build"): ONE source of truth. The pod
# stub (internal/agent/stubbuild.go) embeds this file and scripts/agent-build-sha.sh sources it,
# so the host and the pod build with the same flags. Run it inside the pinned golang image, in
# the source tree, with GOCACHE, GOMODCACHE and HOME fresh. Go builds are byte-identical under
# these flags, which is what lets the host name the expected sha256 without publishing a binary.
# bitc_build COMMIT OUT [ARCH]: build ./cmd/bitc stamped for COMMIT into OUT. With ARCH (amd64 or
# arm64) it cross-compiles for linux/ARCH (CGO is off, so the bytes do not depend on the machine
# that builds); without it the build is native, as the pod's stub does.
bitc_pkg=github.com/Exploded-views/bitc
bitc_build() {
 bitc_c12=$(printf %.12s "$1")
 case ${3-} in
  "") bitc_target= ;;
  amd64|arm64) bitc_target="GOOS=linux GOARCH=$3" ;;
  *) echo "bitc_build: arch must be amd64 or arm64: $3" >&2; return 64 ;;
 esac
 env CGO_ENABLED=0 $bitc_target GOFLAGS=-mod=readonly GOTOOLCHAIN=local go build -trimpath -buildvcs=false \
  -ldflags "-s -w -X $bitc_pkg/internal/version.Version=build-$bitc_c12 -X $bitc_pkg/internal/version.Commit=$bitc_c12" \
  -o "$2" ./cmd/bitc
}
