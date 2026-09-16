package naming

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// Expand substitutes "{key}" placeholders in tmpl with the given values.
// Unknown placeholders are left untouched so misconfiguration is visible in
// the resulting paths rather than silently becoming an empty string.
func Expand(tmpl string, vars map[string]string) string {
	if len(vars) == 0 {
		return tmpl
	}
	pairs := make([]string, 0, len(vars)*2)
	for k, v := range vars {
		pairs = append(pairs, "{"+k+"}", v)
	}
	return strings.NewReplacer(pairs...).Replace(tmpl)
}

// AssetVars builds the placeholder set available in asset selectors and
// archive names.
func AssetVars(v Version) map[string]string {
	return map[string]string{
		"version": v.String(),
		"core":    v.Core(),
		"tag":     v.Tag(),
	}
}

// DirName renders the top-level bundle directory, for example "dbx0.6.14".
func DirName(tmpl string, v Version) string {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = "dbx{version}"
	}
	return Expand(tmpl, AssetVars(v))
}

// SanitizeName reduces a string to characters that are safe in a path segment.
func SanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	return out
}

// BaseName returns the last element of a URL path, percent-decoded and
// sanitized. It is used to keep the original upstream artifact file name
// ("io.dbx.ssh-0.4.76-linux-x64.dbxp").
func BaseName(rawURL string) string {
	trimmed := rawURL
	if i := strings.IndexAny(trimmed, "?#"); i >= 0 {
		trimmed = trimmed[:i]
	}
	base := path.Base(trimmed)
	if base == "." || base == "/" || base == "" {
		return ""
	}
	return SanitizeName(base)
}

// SafeRelPath validates that p can be joined onto the bundle directory without
// escaping it. It returns the cleaned slash-separated relative path.
func SafeRelPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("absolute path not allowed: %q", p)
	}
	cleaned := path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path escapes the bundle directory: %q", p)
	}
	return cleaned, nil
}
