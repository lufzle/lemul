// Package ecs places workspace tasks as Fargate tasks in the customer's VPC.
//
// This is the product's substrate. Everything above it is already exercised by
// the docker driver against the same image, entrypoint, managed settings and
// supervisor binary -- what changes here is only where the container lands and
// which principal it runs as.
//
// Two properties carry the design and neither is incidental:
//
//   - The task role is bedrock/gateway-facing ONLY, and holds no ecs:* at all.
//     The runner holds ecs:RunTask/StopTask on ONE task definition and cannot
//     call Bedrock. Neither escalates into the other (section 2.1). Nothing in
//     this file should ever widen either side.
//   - RunTask is idempotent on clientToken, derived from workspace+generation.
//     A dispatch retried after a timeout must not produce two tasks for one
//     workspace, because that is two filesystems and a split-brain snapshot
//     (section 2.8).
package ecs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/lufzle/lemul/internal/driver"
)

// API is the slice of ECS this driver uses.
//
// An interface rather than the concrete client so the placement logic --
// idempotency, container overrides, the started-by tag -- is testable without an
// AWS account. That matters more here than elsewhere: this is the one driver we
// cannot exercise from the e2e suite.
type API interface {
	RunTask(context.Context, *ecs.RunTaskInput, ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
	StopTask(context.Context, *ecs.StopTaskInput, ...func(*ecs.Options)) (*ecs.StopTaskOutput, error)
	ListTasks(context.Context, *ecs.ListTasksInput, ...func(*ecs.Options)) (*ecs.ListTasksOutput, error)
	// DescribeTasks is how the workspace's disk is found: the EBS volume id is
	// reported on the task as an attachment, and nothing else knows it. ECS
	// mints the volume, so we never chose the id and cannot derive it.
	DescribeTasks(context.Context, *ecs.DescribeTasksInput, ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
}

// EC2API is the slice of EC2 the workspace volume needs.
//
// A SECOND service, and therefore a second widening of section 2.1's runner
// role, which is the thing to weigh before adding anything here. ECS creates
// and attaches the volume on our behalf through the infrastructure role, but it
// will not snapshot one -- so preserving a workspace's disk is an EC2 call the
// runner has to be allowed to make, scoped by the tags ECS puts on the volume.
type EC2API interface {
	CreateSnapshot(context.Context, *ec2.CreateSnapshotInput, ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error)
	DeleteVolume(context.Context, *ec2.DeleteVolumeInput, ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error)
	DeleteSnapshot(context.Context, *ec2.DeleteSnapshotInput, ...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error)
}

// ebsAttachment is the attachment type ECS reports a task's volume under, and
// volumeIDKey the detail that carries the id (ECS API reference, Attachment).
const (
	ebsAttachment = "AmazonElasticBlockStorage"
	volumeIDKey   = "volumeId"
)

// taskStopped is the terminal lastStatus. Reaching it is what detaches the
// volume, which is why anything releasing one has to wait for it.
const taskStopped = "STOPPED"

// startedBy tags every task we place, so ListTasks can find our own tasks
// without depending on bookkeeping that a runner restart would lose.
const startedByPrefix = "lemul-"

type Config struct {
	// Cluster the workspace tasks run in.
	Cluster string
	// TaskDefinition is the ONE family the runner's IAM policy names. Passing a
	// family without a revision lets ECS pick the latest, which is how a new
	// image rolls out without touching the runner's policy.
	TaskDefinition string
	// Subnets must have a route out: the supervisor dials the control plane, and
	// the task pulls its image. Private subnets need a NAT or VPC endpoints.
	Subnets []string
	// SecurityGroups applied to the task ENI. Egress-only is the intent; no
	// ingress rule is needed anywhere, because nothing dials in (section 2.2).
	SecurityGroups []string
	// AssignPublicIP is what a public-subnet deployment needs to reach the
	// registry without a NAT gateway. Private subnets set this false.
	AssignPublicIP bool
	// ContainerName must match the name in the task definition; overrides are
	// addressed by container name, and a mismatch is accepted by the API and
	// then silently ignored at launch.
	ContainerName string

	// VolumeName must match the `volume` declared in the task definition with
	// configuredAtLaunch. Empty disables the workspace volume entirely, and the
	// task's disk is then its ephemeral storage -- which is what every task
	// placed before section 9 item 0 had, and what
	// e2e.TestWithoutAVolumeTheCascadeDestroysEveryHome measures the cost of.
	//
	// ECS matches this by NAME against the task definition and silently attaches
	// nothing when it does not match, so a typo here is a workspace that quietly
	// goes back to losing every home.
	VolumeName string
	// InfrastructureRoleARN is the role ECS assumes to create, attach and tag
	// the volume. Required whenever VolumeName is set; ECS has no default.
	InfrastructureRoleARN string
	// VolumeSizeGiB is the size of a FRESH volume. Ignored when restoring, where
	// the snapshot decides -- passing both is only valid if this is at least the
	// snapshot's size, so it is simply omitted there.
	VolumeSizeGiB int32
	// VolumeType defaults to gp3. `standard` is not supported on Fargate.
	VolumeType string
	// FilesystemType is what ECS formats a fresh volume with, and defaults to
	// ext4. Ignored when restoring, because a snapshot already has one.
	FilesystemType string
	// Encrypted and KMSKeyID encrypt the volume. A workspace disk holds every
	// member's home and conversation history, so this defaults ON.
	Encrypted *bool
	KMSKeyID  string
}

type Driver struct {
	api API
	ec2 EC2API
	cfg Config

	mu sync.Mutex
}

func New(api API, cfg Config) (*Driver, error) {
	switch {
	case cfg.Cluster == "":
		return nil, errors.New("ecs: cluster is required")
	case cfg.TaskDefinition == "":
		return nil, errors.New("ecs: task definition is required")
	case len(cfg.Subnets) == 0:
		return nil, errors.New("ecs: at least one subnet is required")
	}
	if cfg.ContainerName == "" {
		cfg.ContainerName = "workspace"
	}
	if cfg.VolumeName != "" && cfg.InfrastructureRoleARN == "" {
		// Refused here rather than at the first placement. ECS answers a missing
		// roleArn with an InvalidParameterException at RunTask, which surfaces as
		// a workspace that will not start with a message about a field the
		// operator never set -- and placement failures are already the hardest
		// thing in this system to read (section 8).
		return nil, errors.New("ecs: a workspace volume needs an infrastructure role " +
			"for ECS to create and attach it with")
	}
	if cfg.VolumeType == "" {
		cfg.VolumeType = "gp3"
	}
	if cfg.FilesystemType == "" {
		cfg.FilesystemType = "ext4"
	}
	if cfg.Encrypted == nil {
		// On by default: this disk holds every member's home, their config
		// directory and their conversation transcripts.
		on := true
		cfg.Encrypted = &on
	}
	return &Driver{api: api, cfg: cfg}, nil
}

// WithEC2 supplies the client the snapshot verbs need.
//
// Separate from New because it is separate in the deployment too: a driver with
// no EC2 client still places and stops tasks perfectly well, it just cannot
// preserve a disk -- and the Management API discovers that by the driver not
// implementing driver.Snapshotter rather than by a call failing.
func (d *Driver) WithEC2(api EC2API) *Driver {
	d.ec2 = api
	return d
}

func (d *Driver) Name() string { return "ecs" }

// startedBy is the value RunTask records and ListTasks filters on. ECS caps it
// at 36 characters, which is shorter than the 64 the client token allows, so it
// is derived separately rather than reusing the idempotency key.
//
// It is scoped to the GENERATION, not just the workspace, and that is what makes
// adoption safe. Adoption exists so a restarted runner does not place a second
// task for a dispatch it already carried out. Matching on the workspace alone
// made it do something else as well: hand back a task from an EARLIER
// generation -- whose credential the control plane has just invalidated by
// taking this one -- so the placement returned an ARN for a task that could
// never authenticate. Same generation means "I already placed exactly this",
// which is the only case adoption should cover.
//
// The generation is appended last and the workspace id is what gets truncated,
// so the part that distinguishes two placements always survives the 36-character
// cap.
func startedBy(s driver.Spec) string {
	suffix := "-" + itoa(s.Generation)
	head := startedByPrefix + s.WorkspaceID
	if room := 36 - len(suffix); len(head) > room {
		if room < 0 {
			room = 0
		}
		head = head[:room]
	}
	return head + suffix
}

// itoa avoids importing strconv for one call, matching driver.Spec.
func itoa(u uint64) string {
	if u == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}

func (d *Driver) Start(ctx context.Context, s driver.Spec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Adopt a task placed for THIS generation. RunTask's clientToken alone would
	// cover a retried dispatch, but only within its dedupe window, and a runner
	// that restarted has no memory of having placed anything.
	//
	// Scoped to the generation deliberately -- see startedBy. Adopting across
	// generations returns a task whose credential is already invalid.
	if arn, ok := d.existing(ctx, s); ok {
		return arn, nil
	}

	in := &ecs.RunTaskInput{
		Cluster:        aws.String(d.cfg.Cluster),
		TaskDefinition: aws.String(d.cfg.TaskDefinition),
		LaunchType:     types.LaunchTypeFargate,
		Count:          aws.Int32(1),
		// The whole point of section 2.8's generation counter: same workspace,
		// same generation, same token, one task.
		ClientToken: aws.String(s.IdempotencyKey()),
		StartedBy:   aws.String(startedBy(s)),
		NetworkConfiguration: &types.NetworkConfiguration{
			AwsvpcConfiguration: &types.AwsVpcConfiguration{
				Subnets:        d.cfg.Subnets,
				SecurityGroups: d.cfg.SecurityGroups,
				AssignPublicIp: d.assignPublicIP(),
			},
		},
		// Tags are how a customer attributes cost per workspace in their own
		// bill, which is the same question the gateway answers for tokens.
		Tags: []types.Tag{
			{Key: aws.String("lemul:tenant"), Value: aws.String(s.TenantID)},
			{Key: aws.String("lemul:workspace"), Value: aws.String(s.WorkspaceID)},
		},
		Overrides: &types.TaskOverride{
			ContainerOverrides: []types.ContainerOverride{{
				Name: aws.String(d.cfg.ContainerName),
				// The same argv the docker driver passes, because the entrypoint
				// is the same and the supervisor parses these identically.
				Command: []string{
					"-control-plane", s.ControlPlane,
					"-relay", s.Relay,
					"-generation", itoa(s.Generation),
					"-tenant", s.TenantID,
					"-workspace", s.WorkspaceID,
					"-token", s.Credential,
				},
				Environment: environment(s),
			}},
		},
		VolumeConfigurations: d.volumeConfiguration(s),
	}

	out, err := d.api.RunTask(ctx, in)
	if err != nil {
		return "", fmt.Errorf("ecs RunTask: %w", err)
	}
	// RunTask reports per-task failures in a separate field rather than as an
	// error, so a placement that failed for capacity or subnet reasons returns
	// 200 with an empty Tasks list. Treating that as success would hand the
	// control plane a task reference that never existed.
	if len(out.Tasks) == 0 {
		return "", fmt.Errorf("ecs RunTask placed no task: %s", failureText(out.Failures))
	}
	return aws.ToString(out.Tasks[0].TaskArn), nil
}

// volumeConfiguration is the workspace's disk, either fresh or restored.
//
// ECS attaches ONE volume per task and it must be a NEW one -- there is no
// re-attaching the volume the previous task had. So a workspace's filesystem
// outlives its task only as a snapshot, and this is the one place that fact
// turns into behaviour: SnapshotID set means "restore that", empty means "a
// blank disk".
//
// SIZE is omitted when restoring and the FILESYSTEM TYPE is not, and the
// asymmetry is the whole point rather than an oversight. A snapshot carries its
// own size, and sizeInGiB is only valid alongside a snapshot when it is at least
// as large -- so sending a configured default would refuse the placement the
// moment a customer's workspace grew past it, which is the point at which they
// would least like to lose the disk.
//
// The filesystem does NOT work that way, and treating it as if it did cost a
// Fargate round on 2026-08-03. A snapshot carries the bytes of an ext4
// filesystem, but ECS does not read the type back off them -- it is TOLD, and
// "if no value is specified, the xfs filesystem type is used by default"
// (ECS API reference, TaskManagedEBSVolumeConfiguration). So omitting it asked
// ECS to mount an ext4 disk as xfs, which the same reference says plainly: "For
// volumes created from a snapshot, you must specify the same filesystem type
// that the volume was using when the snapshot was created. If there is a
// filesystem type mismatch, the task will fail to start."
//
// It failed exactly like that and said almost nothing: nine minutes in
// PROVISIONING with the volume stuck ATTACHING, then stopCode
// TaskFailedToStart with a null stoppedReason. Nothing local could have caught
// it -- a fake ECS client formats no filesystems -- which is why the restore
// path needed a real account.
func (d *Driver) volumeConfiguration(s driver.Spec) []types.TaskVolumeConfiguration {
	if d.cfg.VolumeName == "" {
		return nil
	}
	vol := &types.TaskManagedEBSVolumeConfiguration{
		RoleArn:    aws.String(d.cfg.InfrastructureRoleARN),
		VolumeType: aws.String(d.cfg.VolumeType),
		Encrypted:  d.cfg.Encrypted,
		// The volume must survive the task, or there is nothing left to
		// snapshot: ECS deletes it on termination by default, and the task is
		// already gone by the time anything of ours notices it stopped.
		TerminationPolicy: &types.TaskManagedEBSVolumeTerminationPolicy{
			DeleteOnTermination: aws.Bool(false),
		},
		TagSpecifications: []types.EBSTagSpecification{{
			ResourceType: types.EBSResourceTypeVolume,
			// The tags are not for a bill. They are what scopes the runner's EC2
			// permissions to volumes ECS made for US, which is the only thing
			// keeping section 2.1's widening narrow.
			Tags: []types.Tag{
				{Key: aws.String("lemul:tenant"), Value: aws.String(s.TenantID)},
				{Key: aws.String("lemul:workspace"), Value: aws.String(s.WorkspaceID)},
			},
		}},
	}
	// Both paths, or the restore mounts as xfs and the task never starts.
	vol.FilesystemType = types.TaskFilesystemType(d.cfg.FilesystemType)
	if s.SnapshotID != "" {
		vol.SnapshotId = aws.String(s.SnapshotID)
	} else {
		vol.SizeInGiB = aws.Int32(d.cfg.VolumeSizeGiB)
	}
	return []types.TaskVolumeConfiguration{{
		Name:             aws.String(d.cfg.VolumeName),
		ManagedEBSVolume: vol,
	}}
}

// Preserve snapshots the disk of the task at ref when keep is true, and
// releases it either way.
//
// THE VOLUME ID IS READ FIRST, before anything waits. It is reported on the
// task and nowhere else, and a task that has finished deprovisioning need not
// still carry the attachment detail -- so reading it after the wait would be
// reading the one field the whole operation depends on at the moment it is
// least likely to be there.
//
// Then it WAITS for the task to actually stop, and that wait is the fix for two
// separate bugs rather than caution. `ecs:StopTask` is asynchronous: it returns
// once the stop is accepted, and on Fargate the task took 46 s from
// `stoppingAt` to `stoppedAt` (measured 2026-08-03). Without the wait:
//
//   - DeleteVolume was rejected `Client.VolumeInUse` every single time, because
//     the volume stays attached until deprovisioning finishes. The error was
//     swallowed as best-effort, so each stop silently orphaned a 30 GiB disk
//     that Terraform never knew about and `terraform destroy` never removed.
//   - The snapshot was CRASH-CONSISTENT, not quiesced. CreateSnapshot began
//     1.3 s before `executionStoppedAt` -- before the container had even
//     stopped running -- so dirty pages still in the host's cache never reached
//     it. Claude Code appends to its transcript as a turn progresses and
//     `--resume` parses that file, so the exposure was a partially written
//     conversation, on the one disk the design exists to protect.
//
// A wait that times out does NOT abort. A crash-consistent snapshot is worth
// far more than no snapshot, and this runs inside a stop the control plane
// bounds at 90 s -- overrunning that would leave the snapshot unrecorded, which
// is the failure that loses the workspace rather than merely leaking a volume.
//
// Returns ("", nil) when there is nothing to preserve. That is not an edge case
// to tidy away: a task that has already gone, or one placed before the volume
// existed, must not stop a workspace from stopping.
func (d *Driver) Preserve(ctx context.Context, ref string, keep bool) (string, error) {
	if d.ec2 == nil || d.cfg.VolumeName == "" || ref == "" {
		return "", nil
	}
	volume, err := d.volumeOf(ctx, ref)
	if err != nil || volume == "" {
		return "", err
	}

	d.awaitStopped(ctx, ref)

	var id string
	if keep {
		out, err := d.ec2.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{
			VolumeId:    aws.String(volume),
			Description: aws.String("lemul workspace " + ref),
			TagSpecifications: []ec2types.TagSpecification{{
				ResourceType: ec2types.ResourceTypeSnapshot,
				Tags: []ec2types.Tag{
					{Key: aws.String("lemul:managed"), Value: aws.String("true")},
				},
			}},
		})
		if err != nil {
			// Nothing is released here. The disk still holds the only copy of
			// every member's home, so releasing it after failing to preserve it
			// would turn a recoverable error into the loss itself.
			return "", fmt.Errorf("ec2 CreateSnapshot %s: %w", volume, err)
		}
		id = aws.ToString(out.SnapshotId)
	}

	// Releasing is best effort and deliberately not fatal, but it is no longer
	// expected to fail: the volume is detached by now. Failing here would
	// discard a snapshot that already exists and hand the caller nothing to
	// record -- losing the workspace to save a few cents.
	//
	// Safe while the snapshot is still pending: a DeleteVolume issued against a
	// volume with a snapshot in progress does not cancel it. The volume sits in
	// `deleting` until the snapshot completes and then goes.
	if _, err := d.ec2.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
		VolumeId: aws.String(volume),
	}); err != nil {
		return id, nil
	}
	return id, nil
}

// stopPollInterval and stopWaitBudget are variables so tests do not sleep.
//
// The budget is bounded well under the control plane's 90 s stop timeout: the
// measured deprovision is ~46 s, and overrunning the caller is worse than an
// unquiesced snapshot, because the reply carrying the snapshot id is what gets
// it recorded.
var (
	stopPollInterval = 3 * time.Second
	stopWaitBudget   = 60 * time.Second
)

// awaitStopped blocks until the task is no longer holding its volume.
//
// Best effort by design, so it returns nothing: every outcome -- stopped,
// budget exhausted, DescribeTasks failing -- leads to the same next step, which
// is to snapshot anyway. A caller that could branch on it would be choosing
// between preserving the disk and not, and there is no input for which "not" is
// the right answer.
func (d *Driver) awaitStopped(ctx context.Context, ref string) {
	deadline := time.Now().Add(stopWaitBudget)
	for {
		out, err := d.api.DescribeTasks(ctx, &ecs.DescribeTasksInput{
			Cluster: aws.String(d.cfg.Cluster),
			Tasks:   []string{ref},
		})
		// A task ECS no longer knows about is a stopped task, not a reason to
		// keep waiting for one.
		if err != nil || len(out.Tasks) == 0 {
			return
		}
		if aws.ToString(out.Tasks[0].LastStatus) == taskStopped {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(stopPollInterval):
		}
	}
}

// DropSnapshot collects the snapshot a newer one replaced.
//
// One that is already gone is success, for the reason Stop treats a missing task
// as success: this runs after a crash as often as it runs in order.
func (d *Driver) DropSnapshot(ctx context.Context, id string) error {
	if d.ec2 == nil || id == "" {
		return nil
	}
	_, err := d.ec2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(id)})
	if err != nil && strings.Contains(err.Error(), "InvalidSnapshot.NotFound") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ec2 DeleteSnapshot %s: %w", id, err)
	}
	return nil
}

// volumeOf reads the workspace volume's id off the task.
//
// ECS mints the volume, so its id is not something we chose or can derive -- it
// is reported on the task as an attachment of type AmazonElasticBlockStorage
// with a `volumeId` detail, and DescribeTasks is the only way to it.
//
// A task with no such attachment returns ("", nil) rather than an error: that is
// what a task placed before the volume existed looks like, and it means "nothing
// to preserve" rather than "something went wrong".
func (d *Driver) volumeOf(ctx context.Context, ref string) (string, error) {
	out, err := d.api.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(d.cfg.Cluster),
		Tasks:   []string{ref},
	})
	if err != nil {
		return "", fmt.Errorf("ecs DescribeTasks %s: %w", ref, err)
	}
	for _, t := range out.Tasks {
		for _, a := range t.Attachments {
			if aws.ToString(a.Type) != ebsAttachment {
				continue
			}
			for _, kv := range a.Details {
				if aws.ToString(kv.Name) == volumeIDKey {
					return aws.ToString(kv.Value), nil
				}
			}
		}
	}
	return "", nil
}

func (d *Driver) assignPublicIP() types.AssignPublicIp {
	if d.cfg.AssignPublicIP {
		return types.AssignPublicIpEnabled
	}
	return types.AssignPublicIpDisabled
}

// environment renders Spec.Env plus the two identifiers the image expects.
//
// Note what is NOT here: no AWS credentials. The task role supplies them through
// the container credential provider, which is the whole reason section 2.1's
// split works -- the sandbox holds precisely the credential it should and
// nothing hands it another (section 2.1).
func environment(s driver.Spec) []types.KeyValuePair {
	env := make([]types.KeyValuePair, 0, len(s.Env)+2)
	for k, v := range s.Env {
		env = append(env, types.KeyValuePair{Name: aws.String(k), Value: aws.String(v)})
	}
	env = append(env,
		types.KeyValuePair{Name: aws.String("LEMUL_TENANT_ID"), Value: aws.String(s.TenantID)},
		types.KeyValuePair{Name: aws.String("LEMUL_WORKSPACE_ID"), Value: aws.String(s.WorkspaceID)},
	)
	return env
}

// existing finds a live task already placed for this workspace.
func (d *Driver) existing(ctx context.Context, s driver.Spec) (string, bool) {
	out, err := d.api.ListTasks(ctx, &ecs.ListTasksInput{
		Cluster:       aws.String(d.cfg.Cluster),
		StartedBy:     aws.String(startedBy(s)),
		DesiredStatus: types.DesiredStatusRunning,
	})
	if err != nil || len(out.TaskArns) == 0 {
		return "", false
	}
	return out.TaskArns[0], true
}

func (d *Driver) Stop(ctx context.Context, ref string) error {
	if ref == "" {
		return nil
	}
	_, err := d.api.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: aws.String(d.cfg.Cluster),
		Task:    aws.String(ref),
		Reason:  aws.String("workspace stopped"),
	})
	if err != nil {
		// Already gone is success. Stop runs on paths that race a task exiting
		// on its own -- an orphaned supervisor exits deliberately (section 2.8).
		var nf *types.InvalidParameterException
		if errors.As(err, &nf) {
			return nil
		}
		return fmt.Errorf("ecs StopTask: %w", err)
	}
	return nil
}

func failureText(fs []types.Failure) string {
	if len(fs) == 0 {
		return "no failures reported"
	}
	var b strings.Builder
	for i, f := range fs {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s: %s", aws.ToString(f.Reason), aws.ToString(f.Detail))
	}
	return b.String()
}
