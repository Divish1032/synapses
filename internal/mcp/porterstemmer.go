// Porter Stemmer — inlined from github.com/reiver/go-porterstemmer (MIT License).
// Implements the Porter stemming algorithm for English word normalization.
// Used by suggestToolsForIntent to match inflected verb forms to keywords.
//
// Original: Martin Porter, 1980. Go implementation: Charles Iliya Krempeaux.
// Inlined to avoid an external dependency for a single function call.
package mcp

import "unicode"

func porterIsConsonant(s []rune, i int) bool {
	switch s[i] {
	case 'a', 'e', 'i', 'o', 'u':
		return false
	case 'y':
		if i == 0 {
			return true
		}
		return !porterIsConsonant(s, i-1)
	default:
		return true
	}
}

func porterMeasure(s []rune) uint {
	lenS := len(s)
	result := uint(0)
	i := 0
	if lenS == 0 {
		return result
	}
	for porterIsConsonant(s, i) {
		i++
		if i >= lenS {
			return result
		}
	}
Outer:
	for i < lenS {
		for !porterIsConsonant(s, i) {
			i++
			if i >= lenS {
				break Outer
			}
		}
		for porterIsConsonant(s, i) {
			i++
			if i >= lenS {
				result++
				break Outer
			}
		}
		result++
	}
	return result
}

func porterHasSuffix(s, suffix []rune) bool {
	lenSMinusOne := len(s) - 1
	lenSuffixMinusOne := len(suffix) - 1
	if lenSMinusOne <= lenSuffixMinusOne {
		return false
	}
	if s[lenSMinusOne] != suffix[lenSuffixMinusOne] {
		return false
	}
	for i := 0; i < lenSuffixMinusOne; i++ {
		if suffix[i] != s[lenSMinusOne-lenSuffixMinusOne+i] {
			return false
		}
	}
	return true
}

func porterContainsVowel(s []rune) bool {
	for i := 0; i < len(s); i++ {
		if !porterIsConsonant(s, i) {
			return true
		}
	}
	return false
}

func porterHasDoubleConsonantSuffix(s []rune) bool {
	lenS := len(s)
	if lenS < 2 {
		return false
	}
	return s[lenS-1] == s[lenS-2] && porterIsConsonant(s, lenS-1)
}

func porterHasCVCSuffix(s []rune) bool {
	lenS := len(s)
	if lenS < 3 {
		return false
	}
	return porterIsConsonant(s, lenS-3) && !porterIsConsonant(s, lenS-2) && porterIsConsonant(s, lenS-1)
}

func porterStep1a(s []rune) []rune {
	lenS := len(s)
	if porterHasSuffix(s, []rune("sses")) {
		return s[:lenS-2]
	} else if porterHasSuffix(s, []rune("ies")) {
		return s[:lenS-2]
	} else if porterHasSuffix(s, []rune("ss")) {
		return s
	} else if porterHasSuffix(s, []rune("s")) {
		return s[:lenS-1]
	}
	return s
}

func porterStep1b(s []rune) []rune {
	lenS := len(s)
	if porterHasSuffix(s, []rune("eed")) {
		sub := s[:lenS-3]
		if porterMeasure(sub) > 0 {
			return s[:lenS-1]
		}
		return s
	}

	var sub []rune
	var matched bool
	if porterHasSuffix(s, []rune("ed")) {
		sub = s[:lenS-2]
		matched = porterContainsVowel(sub)
	} else if porterHasSuffix(s, []rune("ing")) {
		sub = s[:lenS-3]
		matched = porterContainsVowel(sub)
	}
	if !matched {
		return s
	}

	if porterHasSuffix(sub, []rune("at")) || porterHasSuffix(sub, []rune("bl")) || porterHasSuffix(sub, []rune("iz")) {
		return append(sub, 'e')
	}
	c := sub[len(sub)-1]
	if c != 'l' && c != 's' && c != 'z' && porterHasDoubleConsonantSuffix(sub) {
		return sub[:len(sub)-1]
	}
	if porterMeasure(sub) == 1 && porterHasCVCSuffix(sub) && c != 'w' && c != 'x' && c != 'y' {
		return append(sub, 'e')
	}
	return sub
}

func porterStep1c(s []rune) []rune {
	lenS := len(s)
	if lenS < 2 {
		return s
	}
	if s[lenS-1] == 'y' && porterContainsVowel(s[:lenS-1]) {
		s[lenS-1] = 'i'
	} else if s[lenS-1] == 'Y' && porterContainsVowel(s[:lenS-1]) {
		s[lenS-1] = 'I'
	}
	return s
}

func porterStep2(s []rune) []rune {
	lenS := len(s)
	type rule struct {
		suffix      string
		replacement string
		trimLen     int
	}
	rules := []rule{
		{"ational", "ate", 5}, {"tional", "", 2}, {"enci", "ence", 0},
		{"anci", "ance", 0}, {"izer", "ize", 1}, {"bli", "ble", 0},
		{"alli", "al", 2}, {"entli", "ent", 2}, {"eli", "e", 2},
		{"ousli", "ous", 2}, {"ization", "ize", 5}, {"ation", "ate", 3},
		{"ator", "ate", 2}, {"alism", "al", 3}, {"iveness", "ive", 4},
		{"fulness", "ful", 4}, {"ousness", "ous", 4}, {"aliti", "al", 3},
		{"iviti", "ive", 3}, {"biliti", "ble", 5}, {"logi", "log", 1},
	}
	for _, r := range rules {
		sfx := []rune(r.suffix)
		if porterHasSuffix(s, sfx) {
			sub := s[:lenS-len(sfx)]
			if porterMeasure(sub) > 0 {
				if r.replacement != "" {
					return append(sub, []rune(r.replacement)...)
				}
				return s[:lenS-r.trimLen]
			}
			return s
		}
	}
	return s
}

func porterStep3(s []rune) []rune {
	lenS := len(s)
	type rule struct {
		suffix  string
		replace string
	}
	rules := []rule{
		{"icate", "ic"}, {"ative", ""}, {"alize", "al"},
		{"iciti", "ic"}, {"ical", "ic"}, {"ful", ""},
		{"ness", ""},
	}
	for _, r := range rules {
		sfx := []rune(r.suffix)
		if porterHasSuffix(s, sfx) {
			sub := s[:lenS-len(sfx)]
			if porterMeasure(sub) > 0 {
				return append(sub, []rune(r.replace)...)
			}
			return s
		}
	}
	return s
}

func porterStep4(s []rune) []rune {
	lenS := len(s)
	suffixes := []string{
		"al", "ance", "ence", "er", "ic", "able", "ible", "ant",
		"ement", "ment", "ent", "ou", "ism", "ate", "iti", "ous",
		"ive", "ize",
	}
	for _, sfxStr := range suffixes {
		sfx := []rune(sfxStr)
		if porterHasSuffix(s, sfx) {
			sub := s[:lenS-len(sfx)]
			if porterMeasure(sub) > 1 {
				return sub
			}
			return s
		}
	}
	// Special case: "ion" requires preceding s or t.
	if porterHasSuffix(s, []rune("ion")) {
		sub := s[:lenS-3]
		if porterMeasure(sub) > 1 && len(sub) > 0 {
			c := sub[len(sub)-1]
			if c == 's' || c == 't' {
				return sub
			}
		}
	}
	return s
}

func porterStep5a(s []rune) []rune {
	lenS := len(s)
	if s[lenS-1] != 'e' {
		return s
	}
	sub := s[:lenS-1]
	if len(sub) == 0 {
		return s
	}
	m := porterMeasure(sub)
	if m > 1 {
		return sub
	}
	if m == 1 {
		c := sub[len(sub)-1]
		if !porterHasCVCSuffix(sub) || c == 'w' || c == 'x' || c == 'y' {
			return sub
		}
	}
	return s
}

func porterStep5b(s []rune) []rune {
	lenS := len(s)
	if lenS > 2 && s[lenS-2] == 'l' && s[lenS-1] == 'l' {
		sub := s[:lenS-1]
		if porterMeasure(sub) > 1 {
			return sub
		}
	}
	return s
}

// porterStem applies the Porter stemming algorithm to a single word.
// Returns the stemmed form in lowercase.
func porterStem(word string) string {
	s := []rune(word)
	if len(s) <= 2 {
		return word
	}
	for i := range s {
		s[i] = unicode.ToLower(s[i])
	}
	s = porterStep1a(s)
	s = porterStep1b(s)
	s = porterStep1c(s)
	s = porterStep2(s)
	s = porterStep3(s)
	s = porterStep4(s)
	s = porterStep5a(s)
	s = porterStep5b(s)
	return string(s)
}
