package config

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Container is pools.<p>.container: a local pool that runs the admitted command in a container
// (docs/containers.md). Host file only.
type Container struct {
	Image   string
	Runtime string  // "docker"
	Mounts  []Mount // extra bind mounts, after the work tree's
	CPUs    string  // the value for --cpus ("" = no limit)
	Memory  Size    // the value for --memory in bytes (0 = no limit)
}

// Mount is one bind mount: Source on the host at Target in the container.
type Mount struct {
	Source, Target string
	ReadOnly       bool
}

// Runtimes are the container runtimes bitc drives. OrbStack and Docker Desktop provide the docker CLI.
var Runtimes = []string{"docker"}

var imageRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)

// minContainerMemory is docker's own floor for --memory.
const minContainerMemory = Size(6 << 20)

// maxContainerCPUs bounds cpus to something no host has, so a typo (cpus = 1e9) is refused.
const maxContainerCPUs = 4096

func containerOf(p *problems, file, key string, s *containerSchema) *Container {
	c := &Container{Runtime: "docker"}
	switch {
	case s.Image == nil || *s.Image == "":
		p.addf("%s: %s.image is required (an image name such as alpine:3.20)", file, key)
	case !imageRE.MatchString(*s.Image):
		p.addf("%s: %s.image = %q is not an image reference (letters, digits and . _ / : @ -, not starting with -)", file, key, *s.Image)
	default:
		c.Image = *s.Image
	}
	if s.Runtime != nil {
		if !containsString(Runtimes, *s.Runtime) {
			p.addf("%s: %s.runtime = %q must be one of %s", file, key, *s.Runtime, strings.Join(Runtimes, ", "))
		} else {
			c.Runtime = *s.Runtime
		}
	}
	for i, m := range s.Mounts {
		mt, err := parseMount(m)
		if err != "" {
			p.addf("%s: %s.mounts[%d] = %q %s", file, key, i, m, err)
			continue
		}
		c.Mounts = append(c.Mounts, mt)
	}
	switch v := s.Cpus.(type) {
	case nil:
	case int64:
		if v < 1 || v > maxContainerCPUs {
			p.addf("%s: %s.cpus = %d must be a number from 0.01 to %d", file, key, v, maxContainerCPUs)
		} else {
			c.CPUs = strconv.FormatInt(v, 10)
		}
	case float64:
		if math.IsNaN(v) || v < 0.01 || v > maxContainerCPUs {
			p.addf("%s: %s.cpus = %v must be a number from 0.01 to %d", file, key, v, maxContainerCPUs)
		} else {
			c.CPUs = strconv.FormatFloat(v, 'f', -1, 64)
		}
	default:
		p.addf("%s: %s.cpus must be a number (not a string), such as 2 or 1.5", file, key)
	}
	if s.Memory != nil {
		m, err := ParseSize(*s.Memory)
		switch {
		case err != nil:
			p.addf("%s: %s.memory = %q %v", file, key, *s.Memory, err)
		case m < minContainerMemory:
			p.addf("%s: %s.memory = %q is below the 6MiB a container needs", file, key, *s.Memory)
		default:
			c.Memory = m
		}
	}
	return c
}

// parseMount reads "<host path>:<container path>[:ro|:rw]"; both paths absolute and free of
// commas (they go into --mount type=bind,source=...,target=..., a comma-separated list).
func parseMount(s string) (Mount, string) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return Mount{}, `must be "<host path>:<container path>" or "<host path>:<container path>:ro"`
	}
	m := Mount{Source: parts[0], Target: parts[1]}
	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			m.ReadOnly = true
		case "rw":
		default:
			return Mount{}, `has the option ` + strconv.Quote(parts[2]) + `; only ro and rw are allowed`
		}
	}
	for _, p := range []string{m.Source, m.Target} {
		switch {
		case !strings.HasPrefix(p, "/"):
			return Mount{}, "needs absolute paths on both sides"
		case strings.ContainsAny(p, ",\"\n"):
			return Mount{}, "has a path with a comma, a double quote or a newline (docker reads --mount as CSV)"
		}
	}
	return m, ""
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
