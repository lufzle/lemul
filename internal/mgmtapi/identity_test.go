package mgmtapi

import "testing"

// The display name of a personal organization. Not unique and not addressable
// -- the slug is both -- so the only thing that matters is that it reads like a
// person's name and never comes out mangled.
func TestOrgNameForAnEmail(t *testing.T) {
	for _, tc := range []struct{ email, want string }{
		{"dario@sinumo.com", "Dario's Org"},
		{"dario.farzati@acme.io", "Dario's Org"},
		{"dario_farzati@acme.io", "Dario's Org"},
		{"dario+lemul@acme.io", "Dario's Org"},
		{"Dario@acme.io", "Dario's Org"},
		// Already capitalised, and a single letter: both would panic on a naive
		// local[:1] + local[1:] if the slice bounds were wrong.
		{"d@acme.io", "D's Org"},
		// Not an address at all. The identity provider is the one asserting
		// this, so it should not be possible -- but producing "'s Org" would be
		// worse than producing something.
		{"nobody", "Nobody's Org"},
		{"...@acme.io", "My Org"},
	} {
		if got := orgNameFor(tc.email); got != tc.want {
			t.Errorf("orgNameFor(%q) = %q, want %q", tc.email, got, tc.want)
		}
	}
}
