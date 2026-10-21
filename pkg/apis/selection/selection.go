// Package selection holds the selector field types a nodeclass is written with.
package selection

import "strings"

// IDSelector selects exactly one resource by its Oxide UUID. It never wildcards.
// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`
type IDSelector string

// Matches reports whether candidate is the selected ID.
func (id IDSelector) Matches(candidate string) bool {
	return strings.EqualFold(string(id), candidate)
}

// WildcardSelector selects resources by name. A "*" matches any run of characters, so "rke2-*"
// selects every name with that prefix. Without one it is an exact, case-insensitive match.
type WildcardSelector string

// IsPattern reports whether the name wildcards, and so can only be matched against a listing.
func (n WildcardSelector) IsPattern() bool {
	return strings.Contains(string(n), "*")
}

// Matches reports whether candidate satisfies the name.
func (n WildcardSelector) Matches(candidate string) bool {
	pattern := string(n)
	if !n.IsPattern() {
		return strings.EqualFold(pattern, candidate)
	}

	pattern, candidate = strings.ToLower(pattern), strings.ToLower(candidate)
	chunks := strings.Split(pattern, "*")

	if !strings.HasPrefix(candidate, chunks[0]) {
		return false
	}
	candidate = candidate[len(chunks[0]):]
	last := chunks[len(chunks)-1]
	for _, chunk := range chunks[1 : len(chunks)-1] {
		i := strings.Index(candidate, chunk)
		if i < 0 {
			return false
		}
		candidate = candidate[i+len(chunk):]
	}
	return strings.HasSuffix(candidate, last)
}

// VersionSelector selects resources by version. Versions are compared segment by segment, split
// on ".", "-", "_" and spaces with a leading "v" ignored. A "*" segment matches any one segment
// and a trailing "*" matches the rest, so "1.2.*" selects 1.2.3 and 1.2.11 but not 1.2 or 1.3.0.
type VersionSelector string

const versionSeparators = ".-_ \t"

// Matches reports whether candidate satisfies the version.
func (v VersionSelector) Matches(candidate string) bool {
	pattern := string(v)
	if !strings.Contains(pattern, "*") {
		return strings.EqualFold(pattern, candidate)
	}

	patternSegments := splitVersion(pattern)
	versionSegments := splitVersion(candidate)
	if len(patternSegments) == 0 {
		return false
	}

	trailingStar := patternSegments[len(patternSegments)-1] == "*"
	if trailingStar {
		patternSegments = patternSegments[:len(patternSegments)-1]
		if len(versionSegments) <= len(patternSegments) {
			return false
		}
		versionSegments = versionSegments[:len(patternSegments)]
	} else if len(patternSegments) != len(versionSegments) {
		return false
	}

	for i, segment := range patternSegments {
		if segment == "*" {
			continue
		}
		if !strings.EqualFold(segment, versionSegments[i]) {
			return false
		}
	}
	return true
}

func splitVersion(version string) []string {
	version = strings.TrimSpace(version)
	if len(version) > 1 && (version[0] == 'v' || version[0] == 'V') {
		version = version[1:]
	}
	return strings.FieldsFunc(version, func(r rune) bool {
		return strings.ContainsRune(versionSeparators, r)
	})
}
