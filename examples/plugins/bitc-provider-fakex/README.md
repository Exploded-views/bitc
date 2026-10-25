# bitc-provider-fakex

The reference provider plugin for bitc (providers delta A.10.3): a complete, standard-library-only
example of the plugin protocol, written as a client of a small REST API ("fakex"). Copy it as the
starting point of a plugin.

| File | What it shows |
| --- | --- |
| `main.go` | the protocol loop: NDJSON on stdin and stdout, secrets on fd 3, one goroutine per request, `cancel`, exit on EOF |
| `methods.go` | the eleven methods (`hello`, `configure`, `preflight`, `estimate`, `launch`, `watch`, `output`, `fetch`, `owner`, `teardown`, `list`, `spend`) and where a plugin's job ends and bitc core's begins |
| `api.go` | the API client, the mapping of HTTP failures to the protocol's error classes, paginated `list` |
| `faults.go` | the fault modes the conformance suites select (not for copying) |
| `fakeapi/` | the loopback fake of the fakex API (`-bitc <bitc binary>`): each started job runs bitc's own agent. It serves the `/__bitc/` control contract the provider suite uses |

Build and pin it the way a host does:

```sh
CGO_ENABLED=0 go build -trimpath -o bitc-provider-fakex ./examples/plugins/bitc-provider-fakex
sha256sum bitc-provider-fakex   # path + sha256 go into [providers.fakex].plugin
```

Conformance: `go test ./conformance/plugin ./conformance/provider` runs the protocol suite and the
provider suite against this plugin and its fake. Another plugin is tested with
`-plugin=<path> -plugin-sha256=<hex> -plugin-fake=<path>` (provider suite) and
`-plugin=<path> -plugin-config=<json>` (protocol suite).
