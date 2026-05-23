package orgslug

import (
	"strings"
	"testing"
)

// The rule the third word list exists to satisfy. It is arbitrary, which is
// exactly why it needs a test: nothing about "windmill" suggests a constraint,
// so the next person to extend the list will add "willow" and nothing will
// complain.
//
// It asserted the WRONG rule once -- two `l`s and one `a` -- and agreed with a
// list built to match, so "alley" and "allot" passed while having no `m` at
// all. Worth remembering when reading any test that has never failed: it pins
// what was typed, not what was meant.
func TestWordsFollowTheRule(t *testing.T) {
	for _, w := range words {
		if l := strings.Count(w, "l"); l != 2 {
			t.Errorf("%q has %d l's, want exactly 2", w, l)
		}
		if m := strings.Count(w, "m"); m != 1 {
			t.Errorf("%q has %d m's, want exactly 1", w, m)
		}
	}
}

// Every part has to survive a URL path segment and a shell word without
// quoting, since that is where all of them end up.
func TestPartsAreUrlAndShellSafe(t *testing.T) {
	for _, list := range [][]string{numbers, colours, words} {
		for _, s := range list {
			if s == "" {
				t.Fatal("empty entry")
			}
			if strings.ToLower(s) != s {
				t.Errorf("%q is not lowercase", s)
			}
			for _, r := range s {
				if r < 'a' || r > 'z' {
					t.Errorf("%q contains %q, which is not a plain lowercase letter", s, r)
				}
			}
		}
	}
}

// No duplicates: a duplicate silently halves that word's contribution to the
// keyspace and makes the collision rate worse than the list size suggests.
func TestNoDuplicates(t *testing.T) {
	for name, list := range map[string][]string{
		"numbers": numbers, "colours": colours, "words": words,
	} {
		seen := map[string]bool{}
		for _, s := range list {
			if seen[s] {
				t.Errorf("%s: %q appears twice", name, s)
			}
			seen[s] = true
		}
	}
}

func TestGenerateHasThreeParts(t *testing.T) {
	for range 100 {
		s := Generate()
		parts := strings.Split(s, "-")
		if len(parts) != 3 {
			t.Fatalf("Generate() = %q, want three parts", s)
		}
	}
}

// Not a randomness test -- it is a "did someone hardcode an index" test. A
// generator that always returned the same slug would make every sign-up after
// the first fail on the unique constraint, and the retry loop would hide it as
// a slow, eventually-failing sign-up rather than an obvious bug.
func TestGenerateVaries(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		seen[Generate()] = true
	}
	if len(seen) < 100 {
		t.Errorf("200 slugs produced only %d distinct values", len(seen))
	}
}
