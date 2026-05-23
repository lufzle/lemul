// Package wsname generates the two-word name a workspace is given when its
// creator does not pick one: an adjective and a vehicle.
//
//	drifting-schooner
//
// It exists for the same reason internal/orgslug does -- the name is an
// identifier that travels in URLs, in `lem --workspace` and in the supervisor's
// argv, so it has to be URL- and shell-safe and it must not be derived from
// anything a user can change. Two words rather than three keeps it visibly
// different from an organization slug, which shares a URL with it.
//
// Vehicles taken broadly: road, rail, sea, air, snow and worksite, with
// synonyms (`lorry`, `omnibus`, `schooner`, `funicular`) carrying the count
// rather than compounds padding it.
//
// The adjective list contains NO word that turns a vehicle into a disaster --
// no `sinking`, `burning`, `doomed`, `runaway`, `wrecked`, `titanic`. Every one
// of the combinations below ships, so the list cannot contain a word that is
// only wrong in some of them, and `doomed-airliner` sitting in a customer's
// console is a joke we would get to make exactly once. TestNoGrimPairings
// pins it, because the failure mode is somebody adding an ordinary-looking word
// years from now and nobody noticing -- the same reasoning orgslug's house-rule
// test rests on.
package wsname

import (
	"crypto/rand"
	"math/big"
	"strings"
)

// 112 x 105 = 11,760 combinations. Like a slug, a name is never unique by
// construction: UNIQUE (tenant_id, name) decides and the caller retries, which
// is correct at any list size and does not quietly degrade as one fills up.
var (
	adjectives = []string{
		"amber", "ancient", "azure", "balmy", "blithe", "bold", "brave",
		"breezy", "bright", "brisk", "calm", "candid", "cheerful", "chipper",
		"civic", "clever", "coastal", "cosmic", "crimson", "curious", "dapper",
		"daring", "deft", "distant", "drifting", "eager", "early", "elder",
		"ember", "fabled", "fearless", "feathered", "fleet", "fond", "frosty",
		"gallant", "genial", "gentle", "gilded", "gleaming", "golden",
		"graceful", "grand", "hardy", "hazy", "hearty", "humble", "jolly",
		"keen", "kindly", "lively", "lofty", "lucid", "lunar", "mellow", "merry",
		"mild", "misty", "modest", "nimble", "noble", "northern", "olive",
		"opal", "patient", "placid", "plucky", "polar", "prairie", "prime",
		"quiet", "rambling", "restless", "rosy", "rugged", "rustic", "sable",
		"sandy", "serene", "silent", "silver", "sleepy", "snowy", "solar",
		"southern", "spirited", "splendid", "steady", "sterling", "stout",
		"sturdy", "sunlit", "sunny", "sweeping", "tawny", "tidy", "tranquil",
		"trusty", "twilight", "upland", "urban", "valiant", "velvet", "verdant",
		"wandering", "wayward", "western", "willow", "windward", "winsome",
		"wistful", "zesty",
	}

	vehicles = []string{
		"airliner", "airship", "ambulance", "barge", "bicycle", "biplane",
		"blimp", "boat", "bobsled", "boxcar", "brig", "buggy", "bulldozer",
		"bus", "cab", "caboose", "camper", "canoe", "caravan", "carriage",
		"catamaran", "chariot", "clipper", "coach", "convertible", "coupe",
		"crane", "cruiser", "cutter", "dhow", "dinghy", "dogsled", "dragster",
		"dredger", "ferry", "firetruck", "forklift", "freighter", "frigate",
		"funicular", "galleon", "gondola", "gyrocopter", "hatchback",
		"hovercraft", "hydrofoil", "jeep", "jetski", "junk", "kayak", "ketch",
		"limousine", "liner", "locomotive", "lorry", "minivan", "monorail",
		"moped", "motorbike", "omnibus", "outrigger", "oxcart", "pickup", "punt",
		"raft", "railcar", "rickshaw", "roadster", "rocket", "rowboat", "sampan",
		"schooner", "scooter", "seaplane", "sedan", "segway", "shuttle",
		"sidecar", "skiff", "sled", "sleigh", "sloop", "snowmobile", "speedboat",
		"streetcar", "submarine", "subway", "tanker", "taxi", "tender",
		"tractor", "trailer", "tram", "trawler", "tricycle", "trolley",
		"tugboat", "unicycle", "van", "wagon", "warship", "wherry", "yacht",
		"yawl", "zeppelin",
	}
)

// Generate returns a fresh name. Not unique by construction; the database
// decides that, and the caller retries.
func Generate() string {
	return strings.Join([]string{pick(adjectives), pick(vehicles)}, "-")
}

// pick chooses uniformly from a slice.
//
// crypto/rand rather than math/rand, and not because a name is a secret -- it
// is printed in every listing. It is because a name that is guessable in order
// is an enumeration handle, and workspace names are what the explorer endpoints
// and the tunnel registry are keyed on.
func pick(from []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(from))))
	if err != nil {
		// crypto/rand does not fail on any platform this runs on; if it somehow
		// does, a predictable name is worse than a loud one.
		panic("wsname: no randomness available: " + err.Error())
	}
	return from[n.Int64()]
}
