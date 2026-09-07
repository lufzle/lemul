// Package orgslug generates the three-word name an organization is addressed
// by: a number, a colour, and a word.
//
//	forty-crimson-windmill
//
// It is what a user types (`lem --org forty-crimson-windmill`) and the {org}
// segment of every org-scoped URL, so it is generated rather than derived from
// the organization's display name. A display name is not a name in the sense a
// URL needs one: it has apostrophes and spaces ("Ada's Org"), it is not
// unique, and it changes when someone renames it -- which would break every
// bookmark and every script that had the old one.
//
// The third word is constrained to English words with exactly two `l`s and
// exactly one `m`. There is no engineering reason for that; it is a house rule,
// and it is enforced by a test over the list rather than by a comment, because
// the failure mode is somebody adding a perfectly ordinary word years from now
// and nobody noticing.
//
// The list was rebuilt once, because the rule had been written down wrong. The
// comment and the test both said one `a`, and a list satisfying that sailed
// past both -- "alley" and "allot" have no `m` at all. A test that asserts the
// rule you typed rather than the rule you meant will agree with you forever.
package orgslug

import (
	"crypto/rand"
	"math/big"
	"strings"
)

// 90 x 30 x 34 = 91,800 combinations. Collisions are expected rather than
// designed against -- the slug column is UNIQUE and the caller retries, which
// is correct at any list size and does not quietly degrade as one fills up.
var (
	numbers = []string{
		"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
		"eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen",
		"eighteen", "nineteen", "twenty", "twentyone", "twentytwo", "twentythree",
		"twentyfour", "twentyfive", "twentysix", "twentyseven", "twentyeight",
		"twentynine", "thirty", "thirtyone", "thirtytwo", "thirtythree", "thirtyfour",
		"thirtyfive", "thirtysix", "thirtyseven", "thirtyeight", "thirtynine", "forty",
		"fortyone", "fortytwo", "fortythree", "fortyfour", "fortyfive", "fortysix",
		"fortyseven", "fortyeight", "fortynine", "fifty", "fiftyone", "fiftytwo",
		"fiftythree", "fiftyfour", "fiftyfive", "fiftysix", "fiftyseven", "fiftyeight",
		"fiftynine", "sixty", "sixtyone", "sixtytwo", "sixtythree", "sixtyfour",
		"sixtyfive", "sixtysix", "sixtyseven", "sixtyeight", "sixtynine", "seventy",
		"seventyone", "seventytwo", "seventythree", "seventyfour", "seventyfive",
		"seventysix", "seventyseven", "seventyeight", "seventynine", "eighty",
		"eightyone", "eightytwo", "eightythree", "eightyfour", "eightyfive",
		"eightysix", "eightyseven", "eightyeight", "eightynine", "ninety",
	}

	colours = []string{
		"amber", "azure", "beige", "bronze", "cobalt", "copper", "coral", "crimson",
		"cyan", "emerald", "fuchsia", "gold", "indigo", "ivory", "jade", "lilac",
		"magenta", "maroon", "olive", "onyx", "plum", "russet", "saffron", "scarlet",
		"sepia", "sienna", "silver", "teal", "umber", "violet",
	}

	// Nouns, exactly two `l`s and exactly one `m`. Asserted by
	// TestWordsFollowTheRule, which is the only thing keeping this true.
	//
	// Nouns because the slug reads as a thing being named. An adverb in here --
	// `forty-crimson-solemnly` -- parses as a typo rather than as an
	// organization, which is the wrong first impression for the identifier a
	// customer types most often.
	words = []string{
		"allium", "allotment", "allurement", "alluvium", "amaryllis",
		"armadillo", "camellia", "emollient", "gallium", "gristmill",
		"hallmark", "llama", "mallard", "mallet", "mallow", "medallion",
		"milfoil", "millet", "milliner", "millinery", "million", "millpond",
		"millrace", "millstone", "millwright", "mollusk", "mullein", "mullet",
		"mullion", "sawmill", "treadmill", "umbrella", "vellum", "windmill",
	}
)

// Generate returns a fresh slug. It is never unique by construction; the
// database decides that.
func Generate() string {
	return strings.Join([]string{
		pick(numbers), pick(colours), pick(words),
	}, "-")
}

// pick chooses uniformly from a slice.
//
// crypto/rand rather than math/rand, and not because a slug is a secret -- it
// is public and printed everywhere. It is because a slug that is guessable in
// order is an enumeration handle: every organization's URL is derivable from
// the first one you see, which turns "you must know the name" into no
// protection at all the moment anything ever leans on it.
func pick(from []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(from))))
	if err != nil {
		// crypto/rand does not fail on any platform this runs on; if it somehow
		// does, a predictable slug is worse than a loud one.
		panic("orgslug: no randomness available: " + err.Error())
	}
	return from[n.Int64()]
}
