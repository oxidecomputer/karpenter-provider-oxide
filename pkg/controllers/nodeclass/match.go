package nodeclass

import "strings"

// versionSeparators are the characters that delimit segments of an image version string. Oxide
// image versions are free-form, so this covers the formats that show up in practice: semver
// ("1.2.3", "v1.2"), dates ("24.04 2026-05-09"), and bare numbers ("12").
const versionSeparators = ".-_ \t"

// matchVersion reports whether an image's version satisfies the version written in an
// ImageSelector.
//
// A pattern without a "*" is compared literally, case-insensitively, exactly as it was before
// wildcards existed. A pattern containing "*" is instead split into segments on any run of
// versionSeparators and matched segment by segment, where:
//
//   - a "*" in a non-final position matches exactly one segment, so "1.*.3" matches "1.2.3" but
//     never "1.2.3.4";
//   - a trailing "*" matches one or more remaining segments, so "1.*" matches both "1.4" and
//     "1.4.2" but not a bare "1";
//   - a leading "v" is ignored on either side, so "v1.2.*" and "1.2.*" behave identically.
//
// Matching whole segments rather than raw characters is what keeps "1.2.*" from matching "1.20":
// the wildcard cannot consume half of a segment.
func matchVersion(pattern, version string) bool {
	if !strings.Contains(pattern, "*") {
		return strings.EqualFold(pattern, version)
	}

	patternSegments := splitVersion(pattern)
	versionSegments := splitVersion(version)
	if len(patternSegments) == 0 {
		return false
	}

	// A trailing "*" is the only wildcard allowed to span more than one segment, so it is the only
	// case where the two segment counts may differ.
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

// matchName reports whether an image's name satisfies the name written in an ImageSelector.
//
// A pattern without a "*" is compared literally and case-insensitively, which is also the only form
// that can be looked up directly by the Oxide API. A pattern containing "*" is a character-level
// glob, where each "*" matches any run of characters including none, so "rke2-ubuntu-*" and
// "rke2-*" both select "rke2-ubuntu-1780929039".
//
// Unlike a version, a name is matched character by character rather than segment by segment: names
// carry no ordering worth protecting, and prefix selection is the point, so "ubuntu*" should match
// "ubuntu-2404" without the user having to know which separator the name uses.
func matchName(pattern, name string) bool {
	if !strings.Contains(pattern, "*") {
		return strings.EqualFold(pattern, name)
	}

	// Oxide names are ASCII (lowercase letters, digits, and hyphens), so folding the whole string
	// once is equivalent to comparing each chunk case-insensitively.
	pattern, name = strings.ToLower(pattern), strings.ToLower(name)
	chunks := strings.Split(pattern, "*")

	// The text before the first "*" and after the last are anchored; everything between only has to
	// appear in order.
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

// splitVersion breaks a version string into its segments, dropping a leading "v" so that a
// v-prefixed pattern and a bare one select the same images.
func splitVersion(version string) []string {
	version = strings.TrimSpace(version)
	if len(version) > 1 && (version[0] == 'v' || version[0] == 'V') {
		version = version[1:]
	}
	return strings.FieldsFunc(version, func(r rune) bool {
		return strings.ContainsRune(versionSeparators, r)
	})
}
