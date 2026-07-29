// Package territory decides whether a team is allowed to touch a path.
//
// Territory is the cheap half of collision prevention. Most collisions between
// agents are not subtle races over one file; they are two teams that were never
// meant to be in the same directory. Declaring who owns what up front removes
// those without any runtime state, any locking, or any coordination round trip.
//
// The expensive half, time-limited leases on the few files two teams genuinely
// share, only has to cover what is left.
package territory

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Decision is the outcome of a check. Unowned is deliberately distinct from
// Allowed: a path nobody claimed is a gap in the config, and the caller may
// reasonably treat it as permissive on a half-configured repo and as suspicious
// on a fully configured one.
type Decision int

const (
	Allowed Decision = iota
	Denied
	Unowned
	// Shared marks a path both teams may enter, subject to a lease. Territory
	// answers "are you allowed in this room"; the lease answers "is anyone in
	// there right now".
	Shared
)

func (d Decision) String() string {
	switch d {
	case Allowed:
		return "allowed"
	case Denied:
		return "denied"
	case Unowned:
		return "unowned"
	case Shared:
		return "shared"
	}
	return "unknown"
}

// Team is a named group of agents that owns a set of path patterns. A team may
// span vendors: two Claude sessions and a Codex session can all be "backend".
type Team struct {
	Name string
	// Owns are repo-relative glob patterns, matched with the same semantics as
	// filepath.Match per path segment, plus ** to cross segment boundaries.
	Owns []string
	// Shared patterns are enterable by any team, gated by a lease instead.
	Shared []string
}

// Map is the full territory declaration for one repo.
type Map struct {
	Root  string
	Teams []Team
}

var errOutsideRoot = errors.New("path is outside the repo root")

// Check reports whether team may touch path. An empty team name means the agent
// declared no team, which is Unowned rather than Denied: refusing every write
// from an unconfigured agent would make installing this tool break the repo.
func (m *Map) Check(team, path string) (Decision, string, error) {
	rel, err := m.relative(path)
	if err != nil {
		// A path outside the repo is not this tool's business. Leases and
		// territory are per-repo; enforcing beyond the root would mean deciding
		// about the user's whole filesystem.
		return Unowned, "", err
	}

	// Shared wins over ownership. A file listed as shared is shared even if a
	// team also claims the directory above it, because the narrower, explicitly
	// negotiated rule is the one that was written on purpose.
	for _, t := range m.Teams {
		for _, pat := range t.Shared {
			if match(pat, rel) {
				return Shared, pat, nil
			}
		}
	}

	var owner, pattern string
	for _, t := range m.Teams {
		for _, pat := range t.Owns {
			if match(pat, rel) {
				// Two teams claiming the same path is a config bug, and a silent
				// first-wins would hide it until someone lost work.
				if owner != "" && owner != t.Name {
					return Denied, pat, fmt.Errorf(
						"%s is claimed by both %q (%s) and %q (%s)",
						rel, owner, pattern, t.Name, pat)
				}
				owner, pattern = t.Name, pat
			}
		}
	}

	switch {
	case owner == "":
		return Unowned, "", nil
	case team == "":
		return Unowned, pattern, nil
	case owner == team:
		return Allowed, pattern, nil
	default:
		return Denied, pattern, nil
	}
}

// Owner returns the team owning path, or "" when nobody does.
func (m *Map) Owner(path string) string {
	rel, err := m.relative(path)
	if err != nil {
		return ""
	}
	for _, t := range m.Teams {
		for _, pat := range t.Owns {
			if match(pat, rel) {
				return t.Name
			}
		}
	}
	return ""
}

func (m *Map) relative(path string) (string, error) {
	root := filepath.Clean(m.Root)
	p := filepath.Clean(path)
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", fmt.Errorf("%w: %s", errOutsideRoot, path)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", errOutsideRoot, path)
	}
	return rel, nil
}

// match reports whether a repo-relative path matches a glob pattern.
//
// filepath.Match alone is not enough: its * never crosses a separator, so
// "src/api/**" cannot be expressed, and that is the pattern people actually
// write. So ** is handled here and everything else is delegated per segment.
func match(pattern, path string) bool {
	if pattern == "" {
		return false
	}
	// A bare directory pattern owns the directory and everything under it.
	// Writing "src/api" and being told it does not cover "src/api/users.go"
	// is the kind of surprise that gets a tool uninstalled.
	if !strings.ContainsAny(pattern, "*?[") {
		clean := strings.TrimSuffix(pattern, "/")
		return path == clean || strings.HasPrefix(path, clean+"/")
	}
	return matchSegments(
		strings.Split(pattern, "/"),
		strings.Split(path, "/"),
	)
}

func matchSegments(pat, seg []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Trailing ** matches the rest, including nothing.
			if len(pat) == 1 {
				return true
			}
			// Otherwise try every split point.
			for i := 0; i <= len(seg); i++ {
				if matchSegments(pat[1:], seg[i:]) {
					return true
				}
			}
			return false
		}
		if len(seg) == 0 {
			return false
		}
		ok, err := filepath.Match(pat[0], seg[0])
		if err != nil || !ok {
			return false
		}
		pat, seg = pat[1:], seg[1:]
	}
	return len(seg) == 0
}
