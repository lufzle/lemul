package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lufzle/lemul/internal/driver/docker"
	"github.com/lufzle/lemul/internal/driver/local"
)

// The runner's whole job is assembling correct components into a system, and
// until now nothing tested that assembly -- while the libraries underneath it
// are covered thoroughly. That shape is what makes this easy to miss: every part
// is tested and the wiring is not, so a bug here looks like a bug nowhere
// (section 13).

func TestAnUnknownDriverIsRefusedByName(t *testing.T) {
	_, err := newDriver("kubernetes", "./bin/supervisor", "img", "", false)
	if err == nil {
		t.Fatal("an unknown driver name was accepted")
	}
	// Naming it matters: the value came from a flag or a task definition, and
	// "unknown driver" alone sends an operator to read our source.
	if !strings.Contains(err.Error(), "kubernetes") {
		t.Errorf("the refusal does not name what was asked for: %v", err)
	}
}

func TestTheLocalDriverGetsTheSupervisorPath(t *testing.T) {
	d, err := newDriver("local", "/opt/lemul/supervisor", "img", "", false)
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	l, ok := d.(*local.Driver)
	if !ok {
		t.Fatalf("local named a %T", d)
	}
	if l.SupervisorPath != "/opt/lemul/supervisor" {
		t.Errorf("supervisor path %q, want /opt/lemul/supervisor", l.SupervisorPath)
	}
	if d.Name() != "local" {
		t.Errorf("Name() = %q", d.Name())
	}
}

// THE regression, and the reason this file exists.
//
// -project defaulted to the working directory and bind-mounted it at
// /workspace. Nobody had to ask for it: `runner -driver docker` from any
// directory silently mounted that directory into every workspace it placed --
// over the volume root that holds the homes and /shared (section 2.3), so the
// entrypoint then created homes/ and shared/ inside somebody's checkout.
//
// It is also how the docker driver came to report the OPPOSITE of the truth
// about uid isolation: every host path on macOS is virtiofs, which ignores uid
// and gid, so "can Bob read Alice's home?" answered yes locally and no on real
// storage. The workspace mount is a named docker volume now, which carries both
// -- but nothing here may put a host path back at /workspace.
func TestTheDockerDriverMountsNothingItWasNotAsked(t *testing.T) {
	d, err := newDriver("docker", "", "lemul-workspace:dev", "", false)
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	dk, ok := d.(*docker.Driver)
	if !ok {
		t.Fatalf("docker named a %T", d)
	}
	if dk.VolumePrefix != "" {
		t.Errorf("VolumePrefix = %q with none configured; it must not fall back to "+
			"something nobody named", dk.VolumePrefix)
	}
	for _, m := range dk.Mounts {
		if strings.Contains(m, ":/workspace") {
			t.Errorf("mounted %q at /workspace unasked", m)
		}
	}
	if dk.Image != "lemul-workspace:dev" {
		t.Errorf("image %q", dk.Image)
	}
}

func TestTheDockerDriverTakesTheVolumePrefixItWasGiven(t *testing.T) {
	d, err := newDriver("docker", "", "img", "lemul-vol-", false)
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	dk := d.(*docker.Driver)
	if dk.VolumePrefix != "lemul-vol-" {
		t.Errorf("VolumePrefix = %q, want lemul-vol-", dk.VolumePrefix)
	}
}

// The host login is development-only: a workspace running against a customer's
// Bedrock must never see our credentials. Its absence by default is therefore a
// property worth pinning, not just a default.
func TestTheHostLoginIsMountedOnlyWhenAsked(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to mount")
	}

	without, err := newDriver("docker", "", "img", "", false)
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	for _, m := range without.(*docker.Driver).Mounts {
		if strings.Contains(m, filepath.Join(home, ".claude")) {
			t.Fatalf("mounted the host login without -host-login: %q", m)
		}
	}

	with, err := newDriver("docker", "", "img", "", true)
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	found := false
	for _, m := range with.(*docker.Driver).Mounts {
		if strings.Contains(m, filepath.Join(home, ".claude")) {
			found = true
		}
	}
	if !found {
		t.Error("-host-login mounted no login")
	}
}

// Subnets and security groups arrive as one comma-separated string because
// Terraform renders them into the runner's task definition as environment, where
// a list has to be one value. An empty element becomes an empty subnet id, and
// RunTask reports a placement failure in a FIELD rather than as an error -- so
// the symptom is a workspace that never dials in.
func TestSplitListDropsEmptiesAndTrims(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"subnet-a", []string{"subnet-a"}},
		{"subnet-a,subnet-b", []string{"subnet-a", "subnet-b"}},
		{" subnet-a , subnet-b ", []string{"subnet-a", "subnet-b"}},
		{"subnet-a,,subnet-b,", []string{"subnet-a", "subnet-b"}},
	} {
		got := splitList(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitList(%q) = %q, want %q", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitList(%q) = %q, want %q", c.in, got, c.want)
				break
			}
		}
	}
}

func TestEnvOrPrefersTheEnvironment(t *testing.T) {
	t.Setenv("LEMUL_TEST_ENVOR", "")
	if got := envOr("LEMUL_TEST_ENVOR", "fallback"); got != "fallback" {
		t.Errorf("empty value should fall back, got %q", got)
	}
	t.Setenv("LEMUL_TEST_ENVOR", "set")
	if got := envOr("LEMUL_TEST_ENVOR", "fallback"); got != "set" {
		t.Errorf("got %q, want the environment's value", got)
	}
}
