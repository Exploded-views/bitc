package config

import (
	"path"
	"strings"
)

// ValidGlob checks every segment with path.Match.
func ValidGlob(pattern string) error {
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return err
		}
	}
	return nil
}

// Match is path.Match per "/" segment, where a "**" segment matches zero or more segments.
// name is a canonical absolute path.
func Match(pattern, name string) (bool, error) {
	if err := ValidGlob(pattern); err != nil {
		return false, err
	}
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/")), nil
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegs(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}
