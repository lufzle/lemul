// Package local runs a workspace task as a plain child process on our own
// machine.
//
// It is not a toy: it is the environment we debug in, so it goes through the
// same driver interface, the same supervisor binary and the same outbound
// tunnel as the ECS driver. What it skips is only the placement substrate --
// no image, no task definition, no VPC.
//
// A Docker-backed mode using the real sandbox image lands with the image
// itself; the process mode stays, because it is the one that survives having
// no container runtime available.
package local

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"

	"github.com/lufzle/lemul-cc/internal/driver"
)

// Driver spawns one supervisor process per workspace.
type Driver struct {
	// SupervisorPath is the binary to exec.
	SupervisorPath string
	// Stdout and Stderr receive the child's output; nil discards it.
	Stdout, Stderr *os.File

	mu      sync.Mutex
	byKey   map[string]string    // idempotency key -> ref
	byRef   map[string]*exec.Cmd // ref -> process
	nextRef int
}

func New(supervisorPath string) *Driver {
	return &Driver{
		SupervisorPath: supervisorPath,
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
		byKey:          make(map[string]string),
		byRef:          make(map[string]*exec.Cmd),
	}
}

func (d *Driver) Name() string { return "local" }

// Start execs a supervisor. Idempotency is by key rather than by workspace ID:
// a new generation is a deliberate replacement and must place a new task, while
// a retried dispatch of the same generation must not.
func (d *Driver) Start(ctx context.Context, s driver.Spec) (string, error) {
	key := s.IdempotencyKey()

	d.mu.Lock()
	if ref, ok := d.byKey[key]; ok {
		d.mu.Unlock()
		return ref, nil
	}
	d.mu.Unlock()

	args := []string{
		"-control-plane", s.ControlPlane,
		"-tenant", s.TenantID,
		"-workspace", s.WorkspaceID,
		"-token", s.Credential,
	}
	cmd := exec.Command(d.SupervisorPath, args...)
	cmd.Env = append(os.Environ(), envList(s.Env)...)
	cmd.Stdout = d.Stdout
	cmd.Stderr = d.Stderr
	// Own process group, so stopping a workspace does not depend on the
	// runner's own signal disposition and cannot leak into our shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start supervisor: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// Re-check under the lock: a concurrent retry of the same key may have won.
	if ref, ok := d.byKey[key]; ok {
		_ = cmd.Process.Kill()
		go func() { _ = cmd.Wait() }()
		return ref, nil
	}
	d.nextRef++
	ref := "local-" + strconv.Itoa(d.nextRef) + "-pid" + strconv.Itoa(cmd.Process.Pid)
	d.byKey[key] = ref
	d.byRef[ref] = cmd
	// Reap, so a supervisor that exits on its own does not become a zombie.
	go func() { _ = cmd.Wait() }()
	return ref, nil
}

func (d *Driver) Stop(ctx context.Context, ref string) error {
	d.mu.Lock()
	cmd, ok := d.byRef[ref]
	if ok {
		delete(d.byRef, ref)
		for k, v := range d.byKey {
			if v == ref {
				delete(d.byKey, k)
			}
		}
	}
	d.mu.Unlock()
	if !ok || cmd.Process == nil {
		return nil
	}
	// Signal the group: the supervisor may itself have forked PTY children.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// StopAll stops every workspace this driver placed. Used on runner shutdown
// and by tests; the ECS driver has no equivalent because tasks there outlive
// the runner deliberately.
func (d *Driver) StopAll(ctx context.Context) {
	d.mu.Lock()
	refs := make([]string, 0, len(d.byRef))
	for ref := range d.byRef {
		refs = append(refs, ref)
	}
	d.mu.Unlock()
	for _, ref := range refs {
		_ = d.Stop(ctx, ref)
	}
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}
