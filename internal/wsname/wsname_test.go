package wsname

import (
	"strings"
	"testing"
)

// The rule that is not enforceable by reading one word at a time: every
// combination ships, so an adjective is only acceptable if it is acceptable
// against ALL 105 vehicles.
//
// It is a list property rather than a code property, which is exactly why it
// needs a test -- nothing about `drifting` suggests a constraint, so the next
// person to extend the list will add `runaway` and nothing else will complain.
// `doomed-airliner` naming a customer's workspace is not a thing to discover
// from a support ticket.
func TestNoGrimPairings(t *testing.T) {
	banned := []string{
		"sinking", "burning", "doomed", "runaway", "wrecked", "lost",
		"crashing", "stranded", "broken", "failing", "dying", "titanic",
		"haunted", "cursed", "derailed", "capsized", "stalled", "abandoned",
	}
	for _, a := range adjectives {
		for _, b := range banned {
			if a == b {
				t.Errorf("adjective %q makes a disaster of every vehicle it is paired with", a)
			}
		}
	}
}

// Both halves end up in a URL path segment, in a shell word without quoting,
// and in the supervisor's argv. Anything outside plain lowercase letters would
// have to be escaped by three different things that currently do not.
func TestPartsAreUrlAndShellSafe(t *testing.T) {
	for _, list := range [][]string{adjectives, vehicles} {
		for _, s := range list {
			if s == "" {
				t.Fatal("empty entry")
			}
			for _, r := range s {
				if r < 'a' || r > 'z' {
					t.Errorf("%q contains %q, which is not a plain lowercase letter", s, r)
				}
			}
		}
	}
}

// A duplicate silently halves that word's contribution to the keyspace and
// makes collisions more frequent than the list length suggests.
func TestNoDuplicates(t *testing.T) {
	for name, list := range map[string][]string{
		"adjectives": adjectives, "vehicles": vehicles,
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

// Two parts, not three. An organization slug has three, and the two share a
// URL -- `/v1/orgs/{org}/workspaces/{wid}` -- so keeping the shapes distinct is
// what lets a person reading a URL, or a log line, tell which is which.
func TestGenerateHasTwoParts(t *testing.T) {
	for range 100 {
		s := Generate()
		if parts := strings.Split(s, "-"); len(parts) != 2 {
			t.Fatalf("Generate() = %q, want two parts", s)
		}
	}
}

// Not a randomness test -- a "did someone hardcode an index" test. A generator
// returning one value would make every create after the first collide, and the
// retry loop would present that as a slow, eventually-failing create rather
// than as an obvious bug.
func TestGenerateVaries(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		seen[Generate()] = true
	}
	if len(seen) < 100 {
		t.Errorf("200 names produced only %d distinct values", len(seen))
	}
}

// The keyspace the package comment claims. A list that quietly shrank would
// make collisions more common than the retry budget assumes.
func TestKeyspaceIsBigEnoughForTheRetryBudget(t *testing.T) {
	if got := len(adjectives) * len(vehicles); got < 10000 {
		t.Errorf("keyspace is %d combinations, want at least 10,000", got)
	}
}
