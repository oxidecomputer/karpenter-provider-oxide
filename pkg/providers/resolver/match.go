package resolver

import "strings"

const versionSeparators = ".-_ \t"

// MatchVersion reports whether an image's version satisfies the version written in an ImageSelector.
func MatchVersion(pattern, version string) bool {
	if !strings.Contains(pattern, "*") {
		return strings.EqualFold(pattern, version)
	}

	patternSegments := splitVersion(pattern)
	versionSegments := splitVersion(version)
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

// MatchName reports whether a resource's name satisfies the name written in a selector.
func MatchName(pattern, name string) bool {
	if !strings.Contains(pattern, "*") {
		return strings.EqualFold(pattern, name)
	}

	pattern, name = strings.ToLower(pattern), strings.ToLower(name)
	chunks := strings.Split(pattern, "*")

	if !strings.HasPrefix(name, chunks[0]) {
		return false
	}
	name = name[len(chunks[0]):]
	last := chunks[len(chunks)-1]
	for _, chunk := range chunks[1 : len(chunks)-1] {
		i := strings.Index(name, chunk)
		if i < 0 {
			return false
		}
		name = name[i+len(chunk):]
	}
	return strings.HasSuffix(name, last)
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
