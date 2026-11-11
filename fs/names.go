package fs

import (
	"strings"
	"unicode"
)

// foldName returns the key names are compared by: case-insensitive like
// OneDrive, using Unicode simple case folding (the same rule as
// strings.EqualFold), so foldName(a) == foldName(b) exactly when
// strings.EqualFold(a, b).
//
// strings.ToLower was used before, but it maps characters that don't fold to
// each other onto the same letter: Turkish "İ" (U+0130) lowercases to a plain
// "i", so "THİS.txt" and "THIS.txt" collided and one of the two files
// disappeared, and a folder named "İ" could never be found (#390).
func foldName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		b.WriteRune(foldRune(r))
	}
	return b.String()
}

// foldRune returns the smallest rune in r's simple case folding orbit.
func foldRune(r rune) rune {
	min := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < min {
			min = f
		}
	}
	return min
}
