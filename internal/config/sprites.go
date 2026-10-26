package config

// SpritesRefusal is the config problem for any [providers.sprites] table or a pool naming
// sprites on every path but a cleanup: Sprites jobs are refused in this release, with or without
// experimental_remote, until the egress fix (#6) lands and is reviewed. A cleanup (sweep, doctor
// --remote: LoadOptions.Cleanup) gets the provider as CleanupOnly instead.
const SpritesRefusal = "Sprites jobs are refused in this release: their network policy does not restrict " +
	"egress yet, and a job would receive the forwarded environment and the deploy key. `bitc sweep` can " +
	"still clean up Sprites left by 0.1.0 (see https://github.com/Exploded-views/bitc/issues/6 and " +
	"https://github.com/Exploded-views/bitc#known-issues)."

// spritesRefused reports, from the decoded host file alone (nothing is read), whether it
// configures sprites or a pool names it, and is refused for it. Off Linux the platform refusal of
// every provider stands instead. The only bypass is [test] allow_sprites, for the test suites,
// honoured only when every endpoint of the sprites section is a loopback literal (and a [test]
// table needs loopback endpoints anyway, checkTestGate). It keeps a host from using it by
// accident; it is not a security boundary against the host's owner, who may put a loopback proxy
// or tunnel in front of the real API by their own choice.
func spritesRefused(s fileSchema, goos string) bool {
	if goos != "linux" {
		return false
	}
	raw, table := s.Providers["sprites"]
	return spritesNamed(s) && !spritesTestBypass(s, raw, table)
}

// spritesNamed: the host file has a [providers.sprites] table or a pool naming sprites.
func spritesNamed(s fileSchema) bool {
	_, named := s.Providers["sprites"]
	for _, ps := range s.Pools {
		if ps == nil {
			continue
		}
		for _, t := range ps.Providers {
			named = named || t == "sprites"
		}
	}
	return named
}

func spritesTestBypass(s fileSchema, raw map[string]any, table bool) bool {
	if s.Test == nil || s.Test.AllowSprites == nil || !*s.Test.AllowSprites {
		return false
	}
	if !table {
		return true // nothing to talk to (the pool's provider is refused as unconfigured)
	}
	pt, ok := providerTypes["sprites"]
	if !ok || pt.Endpoints == nil {
		return false
	}
	eps, err := pt.Endpoints(raw)
	if err != nil || len(eps) == 0 {
		return false
	}
	for _, ep := range eps {
		if !loopbackLiteral(ep) {
			return false
		}
	}
	return true
}
