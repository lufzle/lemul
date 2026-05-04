// Command runner is the thin entry point for the tenant's control-plane agent.
// The behaviour lives in internal/runner.
//
// Usage:
//
//	eval "$(controlplane -print-runner-env <org-slug>)"
//	runner -control-plane ws://host:9000 -driver local
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/lufzle/lemul/internal/driver"
	"github.com/lufzle/lemul/internal/driver/docker"
	"github.com/lufzle/lemul/internal/driver/local"
	"github.com/lufzle/lemul/internal/logging"
	"github.com/lufzle/lemul/internal/runner"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsdriver "github.com/lufzle/lemul/internal/driver/ecs"
)

// ecsPlacement is the ecs driver's configuration, held at package scope because
// flag.StringVar needs a destination that outlives the var block above.
var ecsCfg struct {
	Cluster               string
	TaskDefinition        string
	Subnets               string
	SecurityGroups        string
	ContainerName         string
	AssignPublicIP        bool
	VolumeName            string
	InfrastructureRoleARN string
}

// ecsVolumeGiB is separate because flag.IntVar needs an int and the driver
// takes an int32.
var ecsVolumeGiB int

func main() {
	var (
		// Defaulted from the environment because Terraform renders both into the
		// runner's own task definition -- the token as a Secrets Manager secret,
		// so it is absent from DescribeTaskDefinition.
		controlPlane = flag.String("control-plane", envOr("LEMUL_CONTROL_PLANE", "ws://localhost:9000"),
			"control plane base URL")
		tenantID = flag.String("tenant", os.Getenv("LEMUL_TENANT_ID"),
			"uuid of the organization this runner serves (required)")
		runnerID = flag.String("id", "", "runner id (defaults to hostname)")
		token    = flag.String("token", os.Getenv("LEMUL_RUNNER_TOKEN"),
			"this organization's runner credential; get it from `controlplane -print-runner-env <org>`")
		driverName   = flag.String("driver", "local", "workspace runtime driver: local|docker|ecs")
		supervisorAt = flag.String("supervisor", "./bin/supervisor", "path to the supervisor binary (local driver)")
		image        = flag.String("image", "lemul-workspace:dev", "sandbox image (docker driver)")
		// One DOCKER NAMED VOLUME per workspace, mounted at /workspace: the
		// local stand-in for the EBS volume a Fargate task gets
		// (docker.Driver.VolumePrefix). Empty disables it.
		//
		// This replaced -project, which bind-mounted a HOST directory at
		// /workspace and defaulted to the working directory. Three things were
		// wrong and only one was ever written down. /workspace stopped being a
		// checkout when a workspace became a shared machine (section 2.3), so
		// the mount landed over the homes root; nobody had to ask for it, so
		// `runner -driver docker` from anywhere mounted that directory into
		// every workspace it placed; and a host path on macOS is virtiofs, which
		// carries neither uid nor the setgid bit nor ACLs -- so /shared came out
		// 775 root:root with no default ACL and every member's home came out
		// root:root, readable by everyone. A named volume is real ext4 and has
		// none of those problems.
		//
		// Volumes OUTLIVE their containers deliberately; that is what carries a
		// member's home across a replacement task. `docker volume ls --filter
		// name=<prefix>` finds them.
		volumePrefix = flag.String("volume-prefix", "lemul-vol-",
			"prefix for the per-workspace docker volume mounted at /workspace (docker driver); empty disables it")
		hostLogin = flag.Bool("host-login", false, "mount the host's Claude login into the container (docker driver, development only)")
	)
	// ECS placement, defaulted from the environment because Terraform delivers it
	// that way: the runner is itself a task, and its task definition is where
	// these values are rendered.
	flag.StringVar(&ecsCfg.Cluster, "ecs-cluster", os.Getenv("LEMUL_ECS_CLUSTER"),
		"cluster workspace tasks run in (ecs driver)")
	flag.StringVar(&ecsCfg.TaskDefinition, "ecs-task-definition", os.Getenv("LEMUL_ECS_TASK_DEFINITION"),
		"the ONE task definition family the runner's IAM policy names (ecs driver)")
	flag.StringVar(&ecsCfg.Subnets, "ecs-subnets", os.Getenv("LEMUL_ECS_SUBNETS"),
		"comma-separated subnet ids for the task ENI (ecs driver)")
	flag.StringVar(&ecsCfg.SecurityGroups, "ecs-security-groups", os.Getenv("LEMUL_ECS_SECURITY_GROUPS"),
		"comma-separated security group ids; egress-only is the intent (ecs driver)")
	flag.StringVar(&ecsCfg.ContainerName, "ecs-container-name", os.Getenv("LEMUL_ECS_CONTAINER_NAME"),
		"container name in the task definition; overrides address it by name (ecs driver)")
	flag.BoolVar(&ecsCfg.AssignPublicIP, "ecs-assign-public-ip", os.Getenv("LEMUL_ECS_ASSIGN_PUBLIC_IP") != "",
		"assign a public IP; needed on public subnets without a NAT gateway (ecs driver)")
	// The workspace volume (section 9 item 0). Empty disables it, which is what
	// every deployment before it had -- and what
	// e2e.TestWithoutAVolumeTheCascadeDestroysEveryHome measures the cost of.
	flag.StringVar(&ecsCfg.VolumeName, "ecs-volume-name", os.Getenv("LEMUL_ECS_VOLUME_NAME"),
		"name of the configuredAtLaunch volume in the task definition; empty disables the workspace volume (ecs driver)")
	flag.StringVar(&ecsCfg.InfrastructureRoleARN, "ecs-infrastructure-role", os.Getenv("LEMUL_ECS_INFRASTRUCTURE_ROLE"),
		"role ECS assumes to create and attach the workspace volume (ecs driver)")
	flag.IntVar(&ecsVolumeGiB, "ecs-volume-gib", envInt("LEMUL_ECS_VOLUME_GIB", 30),
		"size of a FRESH workspace volume; a restored one takes its snapshot's size (ecs driver)")

	flag.Parse()
	logging.Setup("runner")

	// Both required, and refused early rather than at the first dial. The
	// credential is derived FOR an organization, so a runner without one has
	// nothing to present and a runner without the other has nothing to claim --
	// and the symptom either way is a 401 loop that says nothing about which
	// half is missing.
	if *tenantID == "" {
		logging.Fatal("-tenant is required: it is the uuid of the organization whose " +
			"workspaces this runner places, and the credential is derived for it")
	}
	if *token == "" {
		logging.Fatal("-token is required: run `controlplane -print-runner-env <org>` " +
			"to derive this organization's runner credential")
	}

	if *runnerID == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "runner"
		}
		*runnerID = h
	}

	drv, err := newDriver(*driverName, *supervisorAt, *image, *volumePrefix, *hostLogin)
	if err != nil {
		logging.Fatal("building the workspace driver", "driver", *driverName, "error", err)
	}
	slog.Info("runner starting", "driver", drv.Name(), "tenant", *tenantID, "runner", *runnerID)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	r := runner.New(runner.Options{
		ControlPlane: *controlPlane,
		TenantID:     *tenantID,
		RunnerID:     *runnerID,
		Token:        *token,
		Driver:       drv,
	})
	if err := r.Run(ctx); err != nil && ctx.Err() == nil {
		logging.Fatal("runner stopped", "error", err)
	}
}

// newDriver builds the workspace runtime this runner places into.
//
// It is the assembly that both bugs the 2026-08-02 end-user run found lived in
// (section 13): every driver below is covered by its own tests, and choosing
// between them and configuring them was not covered by anything.
func newDriver(name, supervisorPath, image, volumePrefix string, hostLogin bool) (driver.Driver, error) {
	switch name {
	case "local":
		return local.New(supervisorPath), nil
	case "docker":
		d := docker.New(image, docker.DefaultMounts(hostLogin))
		// No fallback to the working directory, and no host path at all. An
		// empty value means no volume, which is the honest answer -- see the
		// flag.
		d.VolumePrefix = volumePrefix
		return d, nil
	case "ecs":
		// Region, credentials and endpoint all come from the ambient AWS config,
		// because in production this process IS the runner task and its identity
		// is the task role -- there is nothing to pass and nothing to store.
		cfg, err := awsconfig.LoadDefaultConfig(context.Background())
		if err != nil {
			return nil, fmt.Errorf("aws config: %w", err)
		}
		d, err := ecsdriver.New(ecs.NewFromConfig(cfg), ecsdriver.Config{
			Cluster:               ecsCfg.Cluster,
			TaskDefinition:        ecsCfg.TaskDefinition,
			Subnets:               splitList(ecsCfg.Subnets),
			SecurityGroups:        splitList(ecsCfg.SecurityGroups),
			AssignPublicIP:        ecsCfg.AssignPublicIP,
			ContainerName:         ecsCfg.ContainerName,
			VolumeName:            ecsCfg.VolumeName,
			InfrastructureRoleARN: ecsCfg.InfrastructureRoleARN,
			//nolint:gosec // a volume size configured by an operator
			VolumeSizeGiB: int32(ecsVolumeGiB),
		})
		if err != nil {
			return nil, err
		}
		// The EC2 client is what makes this driver a driver.Snapshotter, which
		// is how the control plane learns a workspace's disk can be preserved.
		// Same ambient config as ECS: in production this process IS the runner
		// task and its identity is the task role.
		return d.WithEC2(ec2.NewFromConfig(cfg)), nil
	default:
		return nil, fmt.Errorf("unknown driver: %s", name)
	}
}

// splitList parses a comma-separated flag. Subnets and security groups arrive
// this way because Terraform renders them into the runner's task definition as
// environment, where a list has to be one string.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envInt reads an integer flag default from the environment, falling back
// rather than failing: a bad value should not stop a runner starting, and the
// default is a working one.
func envInt(name string, def int) int {
	v, err := strconv.Atoi(os.Getenv(name))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// envOr reads a flag default from the environment, falling back to a value that
// works for local development.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
