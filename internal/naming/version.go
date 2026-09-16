// Package naming turns GitHub tags, release asset names and plugin identifiers
// into the on-disk layout of an offline dbx bundle.
package naming

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a lenient semantic version. dbx releases use MAJOR.MINOR.PATCH
// with an optional pre-release suffix ("0.6.14", "0.7.0-rc.1").
type Version struct {
	Major    int
	Minor    int
	Patch    int
	Pre      string
	Original string
}

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:[-+](.+))?$`)

// ParseVersion accepts "0.6.14", "v0.6.14" or "0.7.0-rc.1" and normalizes it.
func ParseVersion(s string) (Version, error) {
	original := strings.TrimSpace(s)
	trimmed := strings.TrimPrefix(strings.TrimPrefix(original, "v"), "V")
	m := versionRe.FindStringSubmatch(trimmed)
	if m == nil {
		return Version{}, fmt.Errorf("invalid version %q: expected MAJOR.MINOR.PATCH", s)
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return Version{}, fmt.Errorf("invalid major in %q: %w", s, err)
	}
	minor, err := strconv.Atoi(m[2])
	if err != nil {
		return Version{}, fmt.Errorf("invalid minor in %q: %w", s, err)
	}
	patch, err := strconv.Atoi(m[3])
	if err != nil {
		return Version{}, fmt.Errorf("invalid patch in %q: %w", s, err)
	}
	return Version{Major: major, Minor: minor, Patch: patch, Pre: m[4], Original: original}, nil
}

// Core renders "MAJOR.MINOR.PATCH" without any pre-release suffix.
func (v Version) Core() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// String renders the canonical version, including a pre-release suffix and
// without a leading "v". This is the value substituted for "{version}".
func (v Version) String() string {
	if v.Pre != "" {
		return v.Core() + "-" + v.Pre
	}
	return v.Core()
}

// Tag renders the git tag DBX uses for a release.
func (v Version) Tag() string { return "v" + v.String() }

// Compare orders two versions. Pre-releases sort before the final release.
func Compare(a, b Version) int {
	if c := compareInt(a.Major, b.Major); c != 0 {
		return c
	}
	if c := compareInt(a.Minor, b.Minor); c != 0 {
		return c
	}
	if c := compareInt(a.Patch, b.Patch); c != 0 {
		return c
	}
	switch {
	case a.Pre == "" && b.Pre == "":
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	default:
		return strings.Compare(a.Pre, b.Pre)
	}
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// ExtractVersion pulls a version out of a git tag using a regular expression
// that may contain a single capture group.
//
//   - pattern "^v(\d+\.\d+\.\d+)$" against "v0.6.14"      -> "0.6.14"
//   - pattern ""                    against "v0.6.14"      -> "0.6.14" (default)
//   - pattern "^v..."               against "agents-v0.2"  -> no match
//
// It reports false when the tag does not match or does not carry a version.
func ExtractVersion(tag, pattern string) (Version, bool) {
	if pattern == "" {
		pattern = DefaultTagPattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return Version{}, false
	}
	m := re.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, false
	}
	candidate := tag
	if len(m) > 1 {
		candidate = m[1]
	}
	v, err := ParseVersion(candidate)
	if err != nil {
		return Version{}, false
	}
	return v, true
}

// DefaultTagPattern matches the dbx application releases and deliberately
// ignores the other tag families published in the same repository
// ("packages-v0.4.88", "agents-v0.2.111", "agents-latest").
//
// An optional "-suffix" is allowed so that pre-releases such as "v0.7.0-rc.1"
// can be selected by turning on github.include_prerelease; without that flag
// they are filtered out after parsing.
const DefaultTagPattern = `^v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$`
