// Package docker runs a workspace task as a container from the real sandbox
// image.
//
// This is the driver that makes the local environment a genuine rehearsal of
// the product: same image, same entrypoint, same rendered managed settings,
// same supervisor binary, same outbound tunnel. What it skips is only the
// placement substrate -- no task definition, no VPC, no task role.
//
// It is the mitigation for the standing risk in section 13: once the data plane
// lives in customer accounts we cannot attach to a wedged sandbox, so our
// debugging environment has to share the product's code path rather than
// approximate it.
package docker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/lufzle/lemul-cc/internal/driver"
)

// containerPrefix namespaces our containers so Stop and Count cannot touch
// anything else on a developer's machine.
const containerPrefix = "lemul-ws-"

type Driver struct {
	// Image is the sandbox image to run.
	Image string
	// Mounts are extra bind mounts as "host:container[:ro]". Local development
	// uses these to expose the project directory and, when sessions run against
	// a host Claude login rather than Bedrock, the login itself.
	Mounts []string
	// Network, when set, is passed to `docker run --network`.
	Network string

	mu sync.Mutex
}

func New(image string, mounts []string) *Driver {
	return &Driver{Image: image, Mounts: mounts}
}

func (d *Driver) Name() string { return "docker" }

// containerName derives a stable name from the idempotency key. Docker refuses
// duplicate names, so the container runtime itself enforces "one task per
// workspace generation" -- there is no bookkeeping to get wrong or to lose
// across a runner restart, which is more than the process driver can say.
func containerName(s driver.Spec) string {
	return containerPrefix + sanitise(s.IdempotencyKey())
}

func sanitise(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func (d *Driver) Start(ctx context.Context, s driver.Spec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	name := containerName(s)
	if id, ok := d.existing(ctx, name); ok {
		// Same workspace and generation: the dispatch was retried, which is
		// exactly what the idempotency key exists to make harmless (2.8).
		return id, nil
	}

	image := d.Image
	if s.Image != "" {
		image = s.Image
	}
	if image == "" {
		return "", fmt.Errorf("no image configured")
	}

	args := []string{"run", "-d", "--name", name,
		"--label", "lemul.workspace=" + s.WorkspaceID,
		"--label", "lemul.tenant=" + s.TenantID,
	}
	if d.Network != "" {
		args = append(args, "--network", d.Network)
	}
	// Docker Desktop and OrbStack both resolve host.docker.internal, but only
	// Linux needs it added explicitly. Harmless where it is already present.
	args = append(args, "--add-host", "host.docker.internal:host-gateway")
	for _, m := range d.Mounts {
		args = append(args, "-v", m)
	}
	for k, v := range s.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, "-e", "LEMUL_TENANT_ID="+s.TenantID,
		"-e", "LEMUL_WORKSPACE_ID="+s.WorkspaceID)
	if s.MemoryMB > 0 {
		args = append(args, "-m", fmt.Sprintf("%dm", s.MemoryMB))
	}

	args = append(args, image,
		"-control-plane", reachableFromContainer(s.ControlPlane),
		"-tenant", s.TenantID,
		"-workspace", s.WorkspaceID,
		"-token", s.Credential,
	)

	out, err := d.run(ctx, args...)
	if err != nil {
		// A name collision means a concurrent Start won the race; adopt it
		// rather than failing the dispatch.
		if id, ok := d.existing(ctx, name); ok {
			return id, nil
		}
		return "", fmt.Errorf("docker run: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// existing returns the id of a running container with this name.
func (d *Driver) existing(ctx context.Context, name string) (string, bool) {
	out, err := d.run(ctx, "ps", "-aq", "--filter", "name=^"+name+"$")
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(out)
	return id, id != ""
}

func (d *Driver) Stop(ctx context.Context, ref string) error {
	if ref == "" {
		return nil
	}
	if _, err := d.run(ctx, "rm", "-f", ref); err != nil {
		// Already gone is success: Stop is called on paths that can race a
		// container exiting on its own.
		if strings.Contains(err.Error(), "No such container") {
			return nil
		}
		return err
	}
	return nil
}

// StopAll removes every workspace container this driver could have placed.
// Matching on the name prefix rather than on bookkeeping means it also cleans up
// after a runner that was killed rather than stopped.
func (d *Driver) StopAll(ctx context.Context) {
	out, err := d.run(ctx, "ps", "-aq", "--filter", "name=^"+containerPrefix)
	if err != nil {
		return
	}
	for _, id := range strings.Fields(out) {
		_ = d.Stop(ctx, id)
	}
}

func (d *Driver) Count(ctx context.Context) int {
	out, err := d.run(ctx, "ps", "-q", "--filter", "name=^"+containerPrefix)
	if err != nil {
		return 0
	}
	return len(strings.Fields(out))
}

func (d *Driver) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// reachableFromContainer rewrites a control-plane URL that names the host's own
// loopback, which inside a container points at the container itself.
//
// This is the single most likely way a first Docker run fails, and it fails as a
// connection refused that looks like the control plane is down rather than like
// a networking mistake.
func reachableFromContainer(u string) string {
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
		if strings.Contains(u, "//"+host+":") || strings.HasSuffix(u, "//"+host) {
			return strings.Replace(u, "//"+host, "//host.docker.internal", 1)
		}
	}
	return u
}

// DefaultMounts exposes a project directory to the workspace, and the host's
// Claude login when sessions are not using Bedrock.
//
// The login mount is development-only and deliberately conditional: a workspace
// running against a customer's Bedrock must never see it, and a production task
// has no host to mount from in any case.
func DefaultMounts(projectDir string, hostLogin bool) []string {
	var m []string
	if projectDir != "" {
		m = append(m, projectDir+":/workspace")
	}
	if hostLogin {
		if home, err := os.UserHomeDir(); err == nil {
			m = append(m, home+"/.claude:/root/.claude")
			if _, err := os.Stat(home + "/.claude.json"); err == nil {
				m = append(m, home+"/.claude.json:/root/.claude.json")
			}
		}
	}
	return m
}
