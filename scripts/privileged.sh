#!/bin/sh
# privileged.sh: the root-only agent suites in a throwaway container on the
# pinned toolchain image. Maintainers only; never part of `make ci`.
#
# HOST SAFETY: these tests act as root inside a container (users, mounts, namespaces, a firewall).
# They run in exactly two places, both operated by the maintainers (CONTRIBUTING.md, section
# Privileged tests):
#   1. hosted: a manually dispatched workflow on a hosted runner VM with its own kernel that is
#      discarded afterwards (the default);
#   2. rootless: a machine with a rootless container runtime, where container root is an
#      ordinary host user and every capability stays inside a user namespace.
# Anything else is refused, and inside the container the user namespace is checked again before
# any test runs (fail closed). No test may change host-global kernel state.
set -eu

refuse() {
	echo "privileged: refusing to run: $1" >&2
	echo "privileged: these tests are run by the maintainers on a dedicated host; see CONTRIBUTING.md, section Privileged tests" >&2
	exit 2
}

[ -z "${BITC_DEV_IMAGE:-}" ] || refuse "BITC_DEV_IMAGE overrides are not allowed here (the pinned image only)"
top=$(git rev-parse --show-toplevel)
img=$(sed -n 's/^BITC_DEV_IMAGE=\${BITC_DEV_IMAGE:-\(.*\)}$/\1/p' "$top/scripts/dev.sh")
[ -n "$img" ] || refuse "no image pinned in scripts/dev.sh"
[ "$(id -u)" != 0 ] || refuse "running as root"
unset DOCKER_CONTEXT CONTAINER_HOST CONTAINER_CONNECTION

# The runtime and its mode. Hosted: a GitHub-hosted runner, recognised by its fixed user and work
# tree (an environment variable alone is not trusted). Rootless: the per-user Docker socket or a
# local rootless Podman; DOCKER_HOST as found is never used.
mode= rt=
if [ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] &&
	[ "$(id -un)" = runner ] && [ -d /home/runner/work ] && [ "$(stat -c %U /home/runner/work)" = runner ]; then
	# Backstop: a development machine marks itself with this file (created once, outside the repo), and
	# then never takes the hosted path, whatever its environment says.
	[ ! -e "$HOME/.config/bitc/not-a-ci-runner" ] || refuse "hosted mode on a machine marked not-a-ci-runner"
	mode=hosted
	rt="docker -H unix:///var/run/docker.sock"
elif command -v podman >/dev/null 2>&1 && [ "$(podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" = true ]; then
	[ "$(podman info --format '{{.Host.ServiceIsRemote}}' 2>/dev/null)" != true ] || refuse "podman talks to a remote service"
	mode=rootless
	rt=podman
elif command -v docker >/dev/null 2>&1; then
	sock=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/docker.sock
	[ -S "$sock" ] || refuse "no rootless Docker socket at $sock"
	case $(docker -H "unix://$sock" info --format '{{json .SecurityOptions}}' 2>/dev/null) in
	*name=rootless*) ;;
	*) refuse "the daemon at $sock is not rootless" ;;
	esac
	mode=rootless
	rt="docker -H unix://$sock"
else
	refuse "no docker or podman"
fi

# The module cache: the local dev cache (read-only, so run `scripts/dev.sh make test` first), or on a
# hosted runner a fresh one filled by a separate, unprivileged container with the network on.
if [ "$mode" = hosted ]; then
	mod=${RUNNER_TEMP:?}/gomod
	mkdir -p "$mod"
	$rt run --rm --network bridge --user "$(id -u):$(id -g)" -e GOMODCACHE=/go/pkg/mod -e GOTOOLCHAIN=local \
		-e HOME=/tmp -v "$top":/src:ro -v "$mod":/go/pkg/mod -w /src --entrypoint "" "$img" go mod download
else
	mod=${BITC_DEV_CACHE:-$HOME/.cache/bitc-build}/gomod
fi

# Inside the container: in rootless mode refuse unless container root is unprivileged on the host
# (no uid_map or gid_map line may map anything to host id 0); on a hosted runner the VM itself is
# the boundary. Then run the suites.
inner='
set -eu
if [ "$BITC_PRIV_MODE" = rootless ]; then
	for m in /proc/self/uid_map /proc/self/gid_map; do
		awk '"'"'$2 == 0 { bad = 1 } END { exit bad }'"'"' "$m" || {
			echo "privileged: refusing: $m maps container ids to host id 0 (no user namespace)" >&2
			exit 2
		}
	done
fi
go test -tags privileged -count=1 -timeout 15m -v -run TestPrivileged ./internal/agent/ > /tmp/p.log 2>&1 && s=0 || s=$?
cat /tmp/p.log
[ "$s" = 0 ] && ! grep -q -- "--- SKIP" /tmp/p.log
'
# SYS_ADMIN and no AppArmor profile: the exec delivery mounts a tmpfs for the deploy key
# (sprites-provider.md 6.4). Rootless: these exist only inside the container's user namespace.
# Hosted: the runner VM is thrown away after the job.
$rt run --rm --init --network none --pids-limit 512 --memory 4g \
	--cap-add SYS_ADMIN --security-opt apparmor=unconfined --entrypoint "" \
	-e BITC_PRIV_MODE="$mode" -e BITC_PRIVILEGED_TEST=1 -e GOTOOLCHAIN=local \
	-e GOFLAGS="-mod=readonly -buildvcs=false" -e GOPROXY=off \
	-v "$top":/src:ro -v "$mod":/go/pkg/mod:ro -w /src "$img" sh -c "$inner"

# The ssh-host suite (conformance SH rows, Task H.1) turns the container into a whole ssh host:
# sshd, sudo, slot users, an nftables ruleset, veth pairs and network namespaces. It needs network
# administration and an unconfined seccomp profile, so it runs ONLY on the hosted runner, whose VM
# is the boundary and is discarded after the job; a rootless developer machine skips it. The image
# is the pinned toolchain image plus the packages an ssh host has (scripts/privileged.Dockerfile).
# The --sysctl values are per network namespace (this container's), never the runner's.
if [ "$mode" = hosted ]; then
	sshimg=bitc-privileged-ssh:local
	$rt build -q -t "$sshimg" --build-arg BASE="$img" -f "$top/scripts/privileged.Dockerfile" "$top/scripts" >/dev/null
	ssh_inner='
set -eu
go test -tags privileged -count=1 -timeout 25m -v -run "^TestSH" ./conformance/ > /tmp/s.log 2>&1 && s=0 || s=$?
cat /tmp/s.log
[ "$s" = 0 ] && ! grep -q -- "--- SKIP" /tmp/s.log
'
	$rt run --rm --init --network none --pids-limit 1024 --memory 4g \
		--cap-add SYS_ADMIN --cap-add NET_ADMIN --cap-add SYS_RESOURCE \
		--security-opt apparmor=unconfined --security-opt seccomp=unconfined \
		--sysctl net.ipv4.ip_forward=1 --sysctl net.ipv6.conf.all.disable_ipv6=0 \
		--sysctl net.ipv6.conf.default.disable_ipv6=0 --entrypoint "" \
		-e BITC_PRIV_MODE="$mode" -e BITC_PRIVILEGED_TEST=1 -e GOTOOLCHAIN=local \
		-e GOFLAGS="-mod=readonly -buildvcs=false" -e GOPROXY=off \
		-v "$top":/src:ro -v "$mod":/go/pkg/mod:ro -w /src "$sshimg" sh -c "$ssh_inner"
else
	echo "privileged: the ssh-host suite (TestSH*) runs only on the hosted runner; skipped here" >&2
fi
