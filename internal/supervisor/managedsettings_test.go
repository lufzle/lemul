package supervisor

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The managed-settings keys the image writes must be keys Claude Code KNOWS.
//
// This exists because of a measured failure mode rather than a hypothetical one.
// Against the pinned 2.1.220 binary:
//
//   - a bad VALUE makes Claude Code warn loudly and then ignore that field,
//     carrying on without it;
//   - an unrecognised KEY NAME is accepted in complete silence.
//
// So `allowManagedPermissionRulesOnly` misspelled once leaves a workspace with
// no boundary, no error, and nothing in a log to notice. The entrypoint asserts
// the file parses (claude doctor); this asserts the names in it are the ones
// that were actually verified against the binary.
//
// HOW TO VERIFY A NEW KEY, and the correction that cost a wrong conclusion:
// write a value of the WRONG TYPE into managed-settings.json and see whether
// Claude Code rejects it. Rejection proves the key is real.
//
// SILENT ACCEPTANCE PROVES NOTHING. This oracle is one-directional, which was
// not understood when it was first written -- the original note here claimed
// "every real key rejected the same wrong type", and that is false.
// `allowManagedMcpServersOnly` accepts a wrong-typed value in complete silence,
// exactly like an invented control key. Reasoning from that silence to "the key
// does not exist, so MCP cannot be enforced" produced a recorded conclusion that
// was wrong, and cost a round of container measurement to undo.
//
// So: rejection proves a key exists. Silence proves nothing in either direction
// -- and note that "the key exists" is still not "the key does what its name
// says". allowManagedMcpServersOnly turned out to have no demonstrated effect of
// its own; the thing that enforces the MCP allowlist is the presence of a file
// (see TestMCPServersWouldBreakToolDetection). Prove EFFECT behaviourally, one
// variable at a time, or the measurement credits the wrong lever.
var verifiedManagedKeys = map[string]bool{
	// Verified 2026-08-01 against @anthropic-ai/claude-code 2.1.220 by
	// rejection: a wrong-typed value fails schema validation.
	"env":                             true,
	"allowManagedPermissionRulesOnly": true,
	"disableSideloadFlags":            true,
	"permissions":                     true,
	// Accepts a wrong-typed value in silence, so the rejection oracle cannot see
	// it -- and a four-way matrix on 2026-08-02 showed it has NO demonstrated
	// effect on its own either: what refuses a member's MCP servers is the mere
	// existence of /etc/claude-code/managed-mcp.json, with or without this.
	// Kept as depth, not as the mechanism; TestMCPServersWouldBreakToolDetection
	// gates the file, which is the part that holds.
	"allowManagedMcpServersOnly": true,
}

// Nested under `permissions`, verified the same way.
var verifiedPermissionKeys = map[string]bool{
	"additionalDirectories": true,
	"deny":                  true,
	"allow":                 true,
	"ask":                   true,
	"defaultMode":           true,
}

// topLevelKey matches the `key:` lines of the jq object literal the entrypoint
// builds. Reading the script is deliberate: the alternative is asserting against
// a copy of the key list, which would agree with itself forever.
var jqKey = regexp.MustCompile(`(?m)^\s{4}([A-Za-z][A-Za-z0-9_]*):`)

var jqNestedKey = regexp.MustCompile(`(?m)^\s{6}([A-Za-z][A-Za-z0-9_]*):`)

// The Dockerfile line that installs the MCP allowlist where Claude Code reads
// it. Anchored on COPY so a mention in a comment cannot satisfy it.
var copiesManagedMCP = regexp.MustCompile(`(?m)^COPY\s+\S+\s+/etc/claude-code/managed-mcp\.json\s*$`)

func TestTheImageWritesOnlyVerifiedManagedSettingsKeys(t *testing.T) {
	b, err := os.ReadFile("../../image/entrypoint.sh")
	if err != nil {
		t.Skipf("entrypoint not readable from here: %v", err)
	}
	script := string(b)

	// Only the managed-settings object, so the OTel and model-pin `add` calls
	// above it are not mistaken for settings keys.
	start := strings.Index(script, "settings=$(jq -n")
	if start < 0 {
		t.Fatal("could not find the managed-settings object in entrypoint.sh; " +
			"this test is now checking nothing")
	}
	end := strings.Index(script[start:], "}')")
	if end < 0 {
		t.Fatal("could not find the end of the managed-settings object")
	}
	obj := script[start : start+end]

	found := map[string]bool{}
	for _, m := range jqKey.FindAllStringSubmatch(obj, -1) {
		found[m[1]] = true
		if !verifiedManagedKeys[m[1]] {
			t.Errorf("entrypoint.sh writes managed key %q, which is not in the "+
				"verified set -- Claude Code accepts an unknown key silently, so a "+
				"typo here disables the boundary with no error", m[1])
		}
	}
	for _, m := range jqNestedKey.FindAllStringSubmatch(obj, -1) {
		if !verifiedPermissionKeys[m[1]] {
			t.Errorf("entrypoint.sh writes permissions key %q, which is not in the "+
				"verified set", m[1])
		}
	}

	// The two that carry the boundary. Their absence is not a compile error and
	// not a runtime error -- it is just a workspace where members can widen their
	// own permissions and sideload plugins.
	for _, required := range []string{
		"allowManagedPermissionRulesOnly",
		"disableSideloadFlags",
	} {
		if !found[required] {
			t.Errorf("entrypoint.sh no longer writes %s; permission rules MERGE "+
				"across scopes, so without it a member's own settings.json widens "+
				"what the managed file was meant to bound", required)
		}
	}
}

// The precondition tool detection rests on: NO MCP server can run.
//
// Section 2.4's fourth idle condition tells a running tool from a startup child
// by START TIME (activity.go). Measured against the pinned 2.1.220 in the real
// image on 2026-08-02, MCP servers are started LAZILY -- a stdio server
// configured in .claude.json was not spawned by an interactive session sitting
// at its prompt for 40 s, nor by a print-mode turn over 45 s, while
// `claude mcp list` spawned it immediately. So the configuration was good and
// the server simply is not started with the session.
//
// A lazily-started MCP server is long-lived and starts long after any window or
// captured baseline, so it reads as a tool that never finishes -- which reads as
// permanently busy and disables idle detection for that session altogether. That
// is the trap procfs.go's header warns about, reached from the side the header
// did not anticipate, and NEITHER the window nor the baseline design survives
// it.
//
// "No MCP server can run" is ENFORCED rather than conventional, and what
// enforces it is one file at one exact path: /etc/claude-code/managed-mcp.json.
// Its presence alone confines Claude Code to the servers it lists; with the list
// empty, a member's own user-scope `mcpServers` and a project .mcp.json are both
// refused. Measured across all four combinations of that file and the
// `allowManagedMcpServersOnly` setting -- the file decides, the setting does not.
//
// This test gates the path, the list, and the setting, in that order of
// importance. Adding a server -- decision #5's search server is the expected
// one -- must fail here rather than silently switching idle detection off in
// production. The fix at that point is to exclude MCP descendants by their
// configured COMMAND rather than by age, and managed-mcp.json is exactly that
// list.
func TestMCPServersWouldBreakToolDetection(t *testing.T) {
	// THE PATH IS THE MECHANISM. Measured 2026-08-02 across all four
	// combinations of the flag and the file: what refuses a member's MCP
	// servers is the mere existence of /etc/claude-code/managed-mcp.json, and
	// allowManagedMcpServersOnly changes nothing on its own.
	//
	// So this asserts the destination, not just the source file. The image
	// shipped this as mcp.json until 2026-08-02 -- a path Claude Code reads
	// from nowhere -- which is why an empty allowlist sat there for a phase
	// enforcing nothing at all. Renaming it back would reopen the hole in
	// exactly the way that is invisible from the file's contents.
	dockerfile, err := os.ReadFile("../../image/Dockerfile")
	if err != nil {
		t.Fatalf("reading image/Dockerfile: %v", err)
	}
	// The COPY DIRECTIVE, not just the string. Matching the bare path passes on
	// the comment above the directive, which is how the first version of this
	// check sailed through a deliberate revert.
	if !copiesManagedMCP.Match(dockerfile) {
		t.Error("image/Dockerfile no longer installs /etc/claude-code/managed-mcp.json.\n" +
			"That exact path is what enforces the allowlist; anywhere else and " +
			"Claude Code never reads it, so a member's own MCP servers run. See " +
			"the consequence under the allowlist check.")
	}

	// Depth rather than the mechanism, and asserted so it is not dropped by
	// someone reading the matrix above and concluding it does nothing. It may
	// cover routes that matrix did not exercise.
	entry, err := os.ReadFile("../../image/entrypoint.sh")
	if err != nil {
		t.Fatalf("reading entrypoint.sh: %v", err)
	}
	if !strings.Contains(string(entry), "allowManagedMcpServersOnly: true") {
		t.Error("entrypoint.sh no longer sets allowManagedMcpServersOnly. It is " +
			"not what enforces the allowlist -- the managed-mcp.json path is -- " +
			"but it is the setting the binary intends for this, and it costs " +
			"nothing to keep.")
	}

	b, err := os.ReadFile("../../image/managed-mcp.json")
	if err != nil {
		t.Fatalf("reading image/managed-mcp.json: %v", err)
	}
	var doc struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("image/managed-mcp.json is not valid JSON: %v", err)
	}
	if len(doc.MCPServers) != 0 {
		names := make([]string, 0, len(doc.MCPServers))
		for k := range doc.MCPServers {
			names = append(names, k)
		}
		sort.Strings(names)
		t.Fatalf("image/managed-mcp.json now allows MCP servers %v.\n\n"+
			"Section 2.4's \"no tool executing\" condition tells a tool from a "+
			"startup child by START TIME, and that only works while no MCP server "+
			"can run. Measured against 2.1.220: MCP servers start LAZILY, not "+
			"with the session -- so one of these will start outside the window, "+
			"live forever, and read as a tool that never finishes. Every session "+
			"that touches it then looks permanently busy and is never idle-"+
			"stopped, which is a silent regression to unbounded billing and the "+
			"OOM blast radius idle detection exists to bound.\n\n"+
			"Fix supervisor/activity.go first: exclude MCP descendants by their "+
			"configured command. THIS FILE is that command list.", names)
	}
}
