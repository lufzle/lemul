package main

import (
	"os"
	"testing"
)

// TestMain points the user config directory at a throwaway location.
//
// cliauth resolves the token path through os.UserConfigDir, which is a real
// directory on a real machine -- so without this, every test in this package
// reads whatever `ourcli login` last wrote and behaves differently depending on
// whether the person running the suite happens to be signed in. That is exactly
// how TestListSessionsReportsHTTPErrors started failing on a laptop with a
// stale token and passing everywhere else.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ourcli-test")
	if err != nil {
		panic(err)
	}
	// darwin derives it from HOME, linux from XDG_CONFIG_HOME. Set both rather
	// than branch on the platform.
	_ = os.Setenv("HOME", dir)
	_ = os.Setenv("XDG_CONFIG_HOME", dir)

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
