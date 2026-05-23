package ecs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/lufzle/lemul/internal/driver"
)

type fakeAPI struct {
	runs     []*ecs.RunTaskInput
	stops    []*ecs.StopTaskInput
	lists    []*ecs.ListTasksInput
	runOut   *ecs.RunTaskOutput
	runErr   error
	stopErr  error
	listArns []string
	// tasksByStartedBy models what ECS actually does: ListTasks filters on the
	// startedBy value. The flat listArns above answers every query alike, which
	// cannot express "a task exists, but for a different generation" -- the case
	// adoption has to get right.
	tasksByStartedBy map[string][]string
	listErr          error
	listCalls        int
	// describeVolume is the volume id DescribeTasks reports on the task, empty
	// meaning the task carries no EBS attachment -- which is what a task placed
	// before the workspace volume existed looks like.
	describeVolume string
	describeErr    error
	describeCalls  int
	// statuses are the successive lastStatus values DescribeTasks reports, so a
	// test can model what ECS actually does: StopTask is ASYNCHRONOUS, and the
	// task sits in RUNNING/DEPROVISIONING holding its volume for tens of seconds
	// after the call returns. The last value repeats once exhausted.
	//
	// Empty means STOPPED from the first call, which is what every test that is
	// not about the wait wants -- and, more importantly, keeps them from
	// blocking for the whole wait budget.
	statuses []string
	// log, when set, records the order of calls ACROSS both fakes. Ordering is
	// the property under test here: snapshotting before the task stops is a
	// crash-consistent snapshot, and releasing before it stops simply fails.
	log *callLog
}

// callLog records interleaved calls to both fakes, which is the only way to
// assert that the snapshot happened AFTER the task was seen stopped.
type callLog struct {
	mu sync.Mutex
	ev []string
}

func (l *callLog) add(s string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = append(l.ev, s)
}

func (l *callLog) events() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ev...)
}

func (f *fakeAPI) RunTask(_ context.Context, in *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	f.runs = append(f.runs, in)
	if f.runErr != nil {
		return nil, f.runErr
	}
	if f.runOut != nil {
		return f.runOut, nil
	}
	return &ecs.RunTaskOutput{Tasks: []types.Task{{TaskArn: aws.String("arn:aws:ecs:us-east-2:1:task/c/abc")}}}, nil
}

func (f *fakeAPI) StopTask(_ context.Context, in *ecs.StopTaskInput, _ ...func(*ecs.Options)) (*ecs.StopTaskOutput, error) {
	f.stops = append(f.stops, in)
	return &ecs.StopTaskOutput{}, f.stopErr
}

func (f *fakeAPI) ListTasks(_ context.Context, in *ecs.ListTasksInput, _ ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	f.lists = append(f.lists, in)
	f.listCalls++
	if f.tasksByStartedBy != nil {
		return &ecs.ListTasksOutput{
			TaskArns: f.tasksByStartedBy[aws.ToString(in.StartedBy)],
		}, f.listErr
	}
	return &ecs.ListTasksOutput{TaskArns: f.listArns}, f.listErr
}

func (f *fakeAPI) DescribeTasks(_ context.Context, in *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	status := taskStopped
	if len(f.statuses) > 0 {
		if f.describeCalls < len(f.statuses) {
			status = f.statuses[f.describeCalls]
		} else {
			status = f.statuses[len(f.statuses)-1]
		}
	}
	f.describeCalls++
	f.log.add("describe:" + status)
	task := types.Task{TaskArn: aws.String(in.Tasks[0]), LastStatus: aws.String(status)}
	if f.describeVolume != "" {
		task.Attachments = []types.Attachment{
			// An ENI attachment first, because every task has one and picking
			// the first attachment rather than the EBS one would find it.
			{Type: aws.String("ElasticNetworkInterface"), Details: []types.KeyValuePair{
				{Name: aws.String("networkInterfaceId"), Value: aws.String("eni-1")},
			}},
			{Type: aws.String(ebsAttachment), Details: []types.KeyValuePair{
				{Name: aws.String("volumeName"), Value: aws.String("workspace")},
				{Name: aws.String(volumeIDKey), Value: aws.String(f.describeVolume)},
			}},
		}
	}
	return &ecs.DescribeTasksOutput{Tasks: []types.Task{task}}, nil
}

// fakeEC2 records the snapshot lifecycle, which is the half no AWS account is
// available to exercise.
type fakeEC2 struct {
	created   []*ec2.CreateSnapshotInput
	deletedV  []string
	deletedS  []string
	createErr error
	deleteErr error
	log       *callLog
}

func (f *fakeEC2) CreateSnapshot(_ context.Context, in *ec2.CreateSnapshotInput, _ ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error) {
	f.created = append(f.created, in)
	f.log.add("snapshot")
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &ec2.CreateSnapshotOutput{SnapshotId: aws.String("snap-new")}, nil
}

func (f *fakeEC2) DeleteVolume(_ context.Context, in *ec2.DeleteVolumeInput, _ ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error) {
	f.deletedV = append(f.deletedV, aws.ToString(in.VolumeId))
	f.log.add("release")
	return &ec2.DeleteVolumeOutput{}, f.deleteErr
}

func (f *fakeEC2) DeleteSnapshot(_ context.Context, in *ec2.DeleteSnapshotInput, _ ...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error) {
	f.deletedS = append(f.deletedS, aws.ToString(in.SnapshotId))
	return &ec2.DeleteSnapshotOutput{}, f.deleteErr
}

// volumeDriver is testDriver with the workspace volume configured, which is what
// a deployment since section 9 item 0 has.
func volumeDriver(t *testing.T, api API, ec2api EC2API) *Driver {
	t.Helper()
	d, err := New(api, Config{
		Cluster:               "lemul",
		TaskDefinition:        "lemul-workspace",
		Subnets:               []string{"subnet-1"},
		ContainerName:         "workspace",
		VolumeName:            "workspace",
		InfrastructureRoleARN: "arn:aws:iam::1:role/ecsInfrastructureRole",
		VolumeSizeGiB:         30,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d.WithEC2(ec2api)
}

func testDriver(t *testing.T, api API) *Driver {
	t.Helper()
	d, err := New(api, Config{
		Cluster:        "lemul",
		TaskDefinition: "lemul-workspace",
		Subnets:        []string{"subnet-1"},
		SecurityGroups: []string{"sg-1"},
		ContainerName:  "workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func spec() driver.Spec {
	return driver.Spec{
		TenantID:     "t1",
		WorkspaceID:  "w1",
		Generation:   3,
		Credential:   "tunnel-cred",
		ControlPlane: "wss://cp.example/",
		Env:          map[string]string{"LEMUL_GATEWAY_URL": "https://gw:4000"},
	}
}

// New must refuse a configuration that cannot place a task, rather than failing
// at the first dispatch with an AWS error that names none of the missing pieces.
func TestNewRequiresPlacementConfig(t *testing.T) {
	for _, c := range []Config{
		{TaskDefinition: "d", Subnets: []string{"s"}},
		{Cluster: "c", Subnets: []string{"s"}},
		{Cluster: "c", TaskDefinition: "d"},
	} {
		if _, err := New(&fakeAPI{}, c); err == nil {
			t.Errorf("expected an error for %+v", c)
		}
	}
	d, err := New(&fakeAPI{}, Config{Cluster: "c", TaskDefinition: "d", Subnets: []string{"s"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.cfg.ContainerName != "workspace" {
		t.Errorf("container name should default, got %q", d.cfg.ContainerName)
	}
}

// The client token IS the idempotency contract (section 2.8): same workspace and
// generation must yield the same token, and a new generation a different one.
func TestStartSendsTheIdempotencyKeyAsClientToken(t *testing.T) {
	api := &fakeAPI{}
	d := testDriver(t, api)

	if _, err := d.Start(context.Background(), spec()); err != nil {
		t.Fatal(err)
	}
	got := aws.ToString(api.runs[0].ClientToken)
	if want := spec().IdempotencyKey(); got != want {
		t.Fatalf("client token = %q, want %q", got, want)
	}

	next := spec()
	next.Generation = 4
	api.listArns = nil
	if _, err := d.Start(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(api.runs[1].ClientToken) == got {
		t.Fatal("a new generation must not reuse the previous client token")
	}
}

// A runner that restarted has no memory of having placed anything, so adoption
// has to come from ECS rather than from local bookkeeping.
func TestStartAdoptsAnExistingTaskWithoutPlacingASecond(t *testing.T) {
	api := &fakeAPI{listArns: []string{"arn:aws:ecs:us-east-2:1:task/c/existing"}}
	d := testDriver(t, api)

	ref, err := d.Start(context.Background(), spec())
	if err != nil {
		t.Fatal(err)
	}
	if ref != "arn:aws:ecs:us-east-2:1:task/c/existing" {
		t.Fatalf("adopted ref = %q", ref)
	}
	if len(api.runs) != 0 {
		t.Fatalf("placed %d task(s) when one already existed", len(api.runs))
	}
}

// RunTask reports placement failures in a field, not as an error: a capacity or
// subnet problem returns 200 with no tasks. Reporting that as success would hand
// the control plane a reference to a task that never existed.
func TestStartFailsWhenNoTaskWasPlaced(t *testing.T) {
	api := &fakeAPI{runOut: &ecs.RunTaskOutput{
		Failures: []types.Failure{{
			Reason: aws.String("RESOURCE:MEMORY"),
			Detail: aws.String("no container instance met requirements"),
		}},
	}}
	d := testDriver(t, api)

	_, err := d.Start(context.Background(), spec())
	if err == nil {
		t.Fatal("expected an error when RunTask placed no task")
	}
	if !strings.Contains(err.Error(), "RESOURCE:MEMORY") {
		t.Errorf("error should carry the ECS reason, got %v", err)
	}
}

// The supervisor parses the same argv under both drivers, and the task role --
// not the environment -- is what supplies AWS credentials (section 2.1).
func TestStartPassesSupervisorArgvAndNoAWSCredentials(t *testing.T) {
	api := &fakeAPI{}
	d := testDriver(t, api)
	if _, err := d.Start(context.Background(), spec()); err != nil {
		t.Fatal(err)
	}

	ov := api.runs[0].Overrides.ContainerOverrides[0]
	if aws.ToString(ov.Name) != "workspace" {
		t.Errorf("override must address the container by name, got %q", aws.ToString(ov.Name))
	}
	cmd := strings.Join(ov.Command, " ")
	for _, want := range []string{"-control-plane wss://cp.example/", "-tenant t1", "-workspace w1", "-token tunnel-cred"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("argv missing %q, got %q", want, cmd)
		}
	}

	var names []string
	for _, kv := range ov.Environment {
		names = append(names, aws.ToString(kv.Name))
	}
	joined := strings.Join(names, ",")
	for _, forbidden := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("%s must come from the task role, never the environment", forbidden)
		}
	}
	for _, want := range []string{"LEMUL_TENANT_ID", "LEMUL_WORKSPACE_ID", "LEMUL_GATEWAY_URL"} {
		if !strings.Contains(joined, want) {
			t.Errorf("environment missing %q, got %q", want, joined)
		}
	}
}

// StartedBy is capped at 36 characters by the API, shorter than the 64 the
// client token allows -- so it needs its own truncation or RunTask is rejected
// for workspaces with long names.
func TestStartedByFitsTheAPILimit(t *testing.T) {
	s := spec()
	s.WorkspaceID = strings.Repeat("w", 80)
	if got := startedBy(s); len(got) > 36 {
		t.Fatalf("startedBy is %d chars: %q", len(got), got)
	}
	if !strings.HasPrefix(startedBy(spec()), startedByPrefix) {
		t.Error("startedBy must carry the prefix ListTasks filters on")
	}
}

// Truncation must never eat the generation. It is the part that distinguishes
// two placements of one workspace, so losing it silently restores the
// cross-generation adoption this scoping exists to prevent -- and it would only
// show up for workspaces whose names happen to be long.
func TestStartedByKeepsTheGenerationWhenTruncated(t *testing.T) {
	s := spec()
	s.WorkspaceID = strings.Repeat("w", 80)
	s.Generation = 42
	got := startedBy(s)
	if len(got) > 36 {
		t.Fatalf("startedBy is %d chars: %q", len(got), got)
	}
	if !strings.HasSuffix(got, "-42") {
		t.Fatalf("startedBy %q lost the generation to truncation", got)
	}

	s.Generation = 43
	if other := startedBy(s); other == got {
		t.Fatal("two generations of a long workspace id produced the same startedBy")
	}
}

// The composition bug this fixes: the control plane takes a new generation,
// which invalidates the running task's credential, and the driver then adopts
// that very task and reports success. The placement can never work -- the task
// it returned cannot authenticate -- and the sessions inside it die.
func TestAdoptionDoesNotCrossGenerations(t *testing.T) {
	old := spec()
	old.Generation = 3
	api := &fakeAPI{tasksByStartedBy: map[string][]string{
		startedBy(old): {"arn:aws:ecs:us-east-2:1:task/c/gen3"},
	}}
	d := testDriver(t, api)

	next := spec()
	next.Generation = 4
	ref, err := d.Start(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if ref == "arn:aws:ecs:us-east-2:1:task/c/gen3" {
		t.Fatal("adopted a task from an earlier generation, whose credential the " +
			"control plane has already invalidated")
	}
	if len(api.runs) != 1 {
		t.Fatalf("placed %d tasks, want 1", len(api.runs))
	}

	// Same generation still adopts: that is a retried dispatch, which is the
	// case adoption exists for.
	again, err := d.Start(context.Background(), old)
	if err != nil {
		t.Fatal(err)
	}
	if again != "arn:aws:ecs:us-east-2:1:task/c/gen3" {
		t.Fatalf("a same-generation dispatch placed a second task instead of adopting: %q", again)
	}
}

func TestStopIsQuietWhenTheTaskIsAlreadyGone(t *testing.T) {
	api := &fakeAPI{stopErr: &types.InvalidParameterException{}}
	d := testDriver(t, api)

	if err := d.Stop(context.Background(), "arn:task/gone"); err != nil {
		t.Fatalf("already-gone should be success: %v", err)
	}
	if err := d.Stop(context.Background(), ""); err != nil {
		t.Fatalf("empty ref should be a no-op: %v", err)
	}
	if len(api.stops) != 1 {
		t.Fatalf("empty ref must not call StopTask, got %d calls", len(api.stops))
	}

	api2 := &fakeAPI{stopErr: errors.New("boom")}
	d2 := testDriver(t, api2)
	if err := d2.Stop(context.Background(), "arn:task/x"); err == nil {
		t.Fatal("a real failure must surface")
	}
}

// The workspace volume (section 9 item 0). Everything below is the half no AWS
// account is available to exercise, and the half whose failure mode is a
// workspace that comes back blank rather than an error.

// A fresh workspace gets a new volume, and the two settings that make it
// preservable at all: it must not be deleted with the task, and it must be
// tagged, because the tags are what scope the runner's EC2 permissions.
func TestAFreshWorkspaceGetsANewVolume(t *testing.T) {
	api, e := &fakeAPI{}, &fakeEC2{}
	d := volumeDriver(t, api, e)

	if _, err := d.Start(context.Background(), spec()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	vols := api.runs[0].VolumeConfigurations
	if len(vols) != 1 {
		t.Fatalf("RunTask carried %d volume configurations, want 1", len(vols))
	}
	v := vols[0].ManagedEBSVolume
	if aws.ToString(vols[0].Name) != "workspace" {
		t.Errorf("volume name %q; ECS matches this against the task definition by "+
			"NAME and silently attaches nothing when it does not match",
			aws.ToString(vols[0].Name))
	}
	if v.SnapshotId != nil {
		t.Errorf("a workspace with no recorded snapshot restored from %q",
			aws.ToString(v.SnapshotId))
	}
	if aws.ToInt32(v.SizeInGiB) != 30 {
		t.Errorf("sizeInGiB = %d, want 30", aws.ToInt32(v.SizeInGiB))
	}
	if v.FilesystemType != "ext4" {
		t.Errorf("filesystemType = %q", v.FilesystemType)
	}
	// THE setting the whole design rests on. ECS deletes the volume when the
	// task terminates by default, and the task is long gone by the time anything
	// of ours notices -- so with this true there is never anything to snapshot,
	// and every member's home dies with the auto-stop cascade.
	if aws.ToBool(v.TerminationPolicy.DeleteOnTermination) {
		t.Error("deleteOnTermination is true, so the disk goes with the task and " +
			"there is nothing left to snapshot")
	}
}

// Restoring. The snapshot IS the workspace's filesystem between tasks, so this
// is the line that decides whether a stopped workspace comes back or comes back
// blank.
func TestARecordedSnapshotIsRestored(t *testing.T) {
	api, e := &fakeAPI{}, &fakeEC2{}
	d := volumeDriver(t, api, e)

	sp := spec()
	sp.SnapshotID = "snap-abc"
	if _, err := d.Start(context.Background(), sp); err != nil {
		t.Fatalf("Start: %v", err)
	}

	v := api.runs[0].VolumeConfigurations[0].ManagedEBSVolume
	if aws.ToString(v.SnapshotId) != "snap-abc" {
		t.Fatalf("snapshotId = %q, want snap-abc -- the workspace comes back empty",
			aws.ToString(v.SnapshotId))
	}
	// SIZE must not be sent alongside a snapshot: it is only valid there when it
	// is at least the snapshot's size, so a configured default refuses the
	// placement the moment a workspace outgrows it -- which is exactly when
	// losing the disk would hurt most.
	if v.SizeInGiB != nil {
		t.Errorf("sizeInGiB %d sent with a snapshot; it refuses the placement once "+
			"the disk outgrows the configured size", aws.ToInt32(v.SizeInGiB))
	}
	// The FILESYSTEM TYPE must be, and this assertion used to say the opposite
	// -- it required the field to be empty, so it would have failed the fix
	// rather than the bug. The belief behind it was that a snapshot carries its
	// filesystem the way it carries its size. It carries the bytes; ECS does not
	// read the type off them. "If no value is specified, the xfs filesystem type
	// is used by default", and "for volumes created from a snapshot, you must
	// specify the same filesystem type that the volume was using when the
	// snapshot was created. If there is a filesystem type mismatch, the task
	// will fail to start" (ECS API reference).
	//
	// Measured on Fargate 2026-08-03 before this changed: the restored task sat
	// nine minutes in PROVISIONING with its volume ATTACHING and then stopped
	// with TaskFailedToStart and a null stoppedReason. A workspace that stops
	// never comes back -- the exact loss section 9 item 0 exists to prevent,
	// reintroduced one field away from where it was fixed.
	if v.FilesystemType != "ext4" {
		t.Errorf("filesystemType = %q, want ext4; ECS defaults a snapshot-restored "+
			"volume to xfs and the task then fails to start", v.FilesystemType)
	}
}

// No volume configured is the pre-section-9 deployment, and it must keep
// placing tasks rather than refusing them.
func TestWithoutAVolumeNameRunTaskCarriesNoVolume(t *testing.T) {
	api := &fakeAPI{}
	d := testDriver(t, api)

	if _, err := d.Start(context.Background(), spec()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := api.runs[0].VolumeConfigurations; got != nil {
		t.Errorf("RunTask carried %d volume configurations with none configured", len(got))
	}
}

// A volume with no infrastructure role is refused at construction. ECS answers
// it with an InvalidParameterException at RunTask instead, which reaches an
// operator as a workspace that will not start, blaming a field they never set.
func TestAVolumeNeedsAnInfrastructureRole(t *testing.T) {
	_, err := New(&fakeAPI{}, Config{
		Cluster: "c", TaskDefinition: "d", Subnets: []string{"s"},
		VolumeName: "workspace",
	})
	if err == nil {
		t.Fatal("a volume was accepted with no infrastructure role")
	}
	if !strings.Contains(err.Error(), "infrastructure role") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}
}

// Snapshot: find the volume on the task, snapshot it, then drop the volume.
//
// The order is what makes it safe, and it is safe rather than hopeful: a
// DeleteVolume issued while a snapshot is pending does not cancel it -- the
// volume waits in `deleting` until the snapshot completes. So this needs no
// polling loop, which matters because the caller is the auto-stop sweeper.
func TestSnapshotPreservesTheDiskThenReleasesTheVolume(t *testing.T) {
	api := &fakeAPI{describeVolume: "vol-123"}
	e := &fakeEC2{}
	d := volumeDriver(t, api, e)

	id, err := d.Preserve(context.Background(), "arn:task/abc", true)
	if err != nil {
		t.Fatalf("Preserve: %v", err)
	}
	if id != "snap-new" {
		t.Fatalf("snapshot id %q", id)
	}
	if len(e.created) != 1 || aws.ToString(e.created[0].VolumeId) != "vol-123" {
		t.Fatalf("snapshotted %v, want vol-123", e.created)
	}
	if len(e.deletedV) != 1 || e.deletedV[0] != "vol-123" {
		t.Errorf("released %v, want vol-123 -- a volume left behind bills forever", e.deletedV)
	}
}

// A task with no EBS attachment is "nothing to preserve", not a failure. That
// is what a task placed before the volume existed looks like, and a workspace
// must still be allowed to stop.
func TestSnapshotOfATaskWithNoVolumeIsNotAnError(t *testing.T) {
	api := &fakeAPI{} // no describeVolume
	e := &fakeEC2{}
	d := volumeDriver(t, api, e)

	id, err := d.Preserve(context.Background(), "arn:task/abc", true)
	if err != nil {
		t.Fatalf("Preserve: %v", err)
	}
	if id != "" {
		t.Errorf("snapshot id %q from a task with no volume", id)
	}
	if len(e.created) != 0 {
		t.Error("snapshotted something on a task with no volume attachment")
	}
}

// A volume that cannot be released must still yield its snapshot. Failing here
// would discard a snapshot that already exists and hand the caller nothing to
// record -- losing the workspace to save the cost of one volume.
func TestAnUnreleasableVolumeStillYieldsItsSnapshot(t *testing.T) {
	api := &fakeAPI{describeVolume: "vol-123"}
	e := &fakeEC2{deleteErr: errors.New("VolumeInUse")}
	d := volumeDriver(t, api, e)

	id, err := d.Preserve(context.Background(), "arn:task/abc", true)
	if err != nil {
		t.Fatalf("Preserve: %v", err)
	}
	if id != "snap-new" {
		t.Errorf("a failed DeleteVolume lost the snapshot: %q", id)
	}
}

// DropSnapshot collects the one a newer snapshot replaced, and treats one that
// is already gone as success -- it runs after a crash as often as in order.
func TestDropSnapshotIsIdempotent(t *testing.T) {
	e := &fakeEC2{}
	d := volumeDriver(t, &fakeAPI{}, e)
	ctx := context.Background()

	if err := d.DropSnapshot(ctx, "snap-old"); err != nil {
		t.Fatalf("DropSnapshot: %v", err)
	}
	if len(e.deletedS) != 1 || e.deletedS[0] != "snap-old" {
		t.Fatalf("deleted %v", e.deletedS)
	}
	if err := d.DropSnapshot(ctx, ""); err != nil {
		t.Errorf("dropping no snapshot: %v", err)
	}

	gone := volumeDriver(t, &fakeAPI{},
		&fakeEC2{deleteErr: errors.New("InvalidSnapshot.NotFound: no such snapshot")})
	if err := gone.DropSnapshot(ctx, "snap-old"); err != nil {
		t.Errorf("deleting an already-deleted snapshot: %v", err)
	}
}

// fastWait shrinks the stop wait so tests do not sleep for the real budget.
func fastWait(t *testing.T) {
	t.Helper()
	poll, budget := stopPollInterval, stopWaitBudget
	stopPollInterval, stopWaitBudget = time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { stopPollInterval, stopWaitBudget = poll, budget })
}

// THE ORDERING FIX. ecs:StopTask is asynchronous -- it returns once the stop is
// accepted, and on Fargate the task held its volume for another 46 s. Acting
// immediately produced two bugs at once: DeleteVolume was rejected VolumeInUse
// every time and the orphaned disk was swallowed as best effort, and the
// snapshot was taken 1.3 s BEFORE the container stopped executing, making it
// crash-consistent rather than quiesced.
//
// So the volume id is read first, while the task still reports the attachment,
// and nothing is snapshotted or released until the task says STOPPED.
func TestPreserveWaitsForTheTaskToStopBeforeTouchingTheDisk(t *testing.T) {
	fastWait(t)
	log := &callLog{}
	api := &fakeAPI{
		describeVolume: "vol-123",
		statuses:       []string{"RUNNING", "DEPROVISIONING", taskStopped},
		log:            log,
	}
	e := &fakeEC2{log: log}
	d := volumeDriver(t, api, e)

	if _, err := d.Preserve(context.Background(), "arn:task/abc", true); err != nil {
		t.Fatalf("Preserve: %v", err)
	}

	got := log.events()
	want := []string{
		"describe:RUNNING",        // reads the volume id, while it is still there
		"describe:DEPROVISIONING", // still holding the disk
		"describe:" + taskStopped,
		"snapshot",
		"release",
	}
	if len(got) != len(want) {
		t.Fatalf("call order %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call order %v, want %v", got, want)
		}
	}
}

// A DELETE is the case that orphaned a volume unconditionally: it passes
// keep=false precisely to avoid billing for a disk the customer asked us to
// destroy, and releasing used to live INSIDE the snapshot call -- so the whole
// step was skipped and the volume outlived the workspace.
//
// "Do not preserve" must never mean "do not release".
func TestPreserveReleasesTheVolumeItWasToldNotToKeep(t *testing.T) {
	fastWait(t)
	api := &fakeAPI{describeVolume: "vol-123"}
	e := &fakeEC2{}
	d := volumeDriver(t, api, e)

	id, err := d.Preserve(context.Background(), "arn:task/abc", false)
	if err != nil {
		t.Fatalf("Preserve: %v", err)
	}
	if id != "" {
		t.Errorf("snapshot id %q from a delete; that is a bill for a disk the "+
			"customer asked us to destroy", id)
	}
	if len(e.created) != 0 {
		t.Errorf("snapshotted %d time(s) on a delete", len(e.created))
	}
	if len(e.deletedV) != 1 || e.deletedV[0] != "vol-123" {
		t.Fatalf("released %v, want vol-123 -- a delete that leaves its volume "+
			"behind orphans the disk it was trying not to pay for", e.deletedV)
	}
}

// The one ordering that must NOT hold. A disk holds the only copy of every
// member's home until its snapshot exists, so a failed CreateSnapshot has to
// leave the volume alone -- releasing there turns a retryable error into the
// loss itself.
func TestPreserveKeepsADiskItCouldNotSnapshot(t *testing.T) {
	fastWait(t)
	api := &fakeAPI{describeVolume: "vol-123"}
	e := &fakeEC2{createErr: errors.New("SnapshotCreationPerVolumeRateExceeded")}
	d := volumeDriver(t, api, e)

	if _, err := d.Preserve(context.Background(), "arn:task/abc", true); err == nil {
		t.Fatal("a failed snapshot was reported as success")
	}
	if len(e.deletedV) != 0 {
		t.Fatalf("released %v after failing to snapshot it -- that is the data "+
			"loss, not a leaked volume", e.deletedV)
	}
}

// A task that never reports STOPPED must not hold the stop open. The control
// plane bounds this call at 90 s, and overrunning it leaves the snapshot
// unrecorded -- which loses the workspace, where an unquiesced snapshot merely
// risks a partial last write.
func TestPreserveGivesUpWaitingRatherThanOverrunningTheStop(t *testing.T) {
	fastWait(t)
	api := &fakeAPI{describeVolume: "vol-123", statuses: []string{"RUNNING"}}
	e := &fakeEC2{}
	d := volumeDriver(t, api, e)

	done := make(chan string, 1)
	go func() {
		id, _ := d.Preserve(context.Background(), "arn:task/abc", true)
		done <- id
	}()
	select {
	case id := <-done:
		if id != "snap-new" {
			t.Fatalf("snapshot id %q; giving up on the wait must still preserve "+
				"the disk", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Preserve never returned on a task that never stops")
	}
}

// The ecs driver is the only one that can preserve a disk, and the Management
// API discovers that by asking rather than by knowing which driver it holds.
func TestTheDriverAnnouncesThatItSnapshots(t *testing.T) {
	if _, ok := any(volumeDriver(t, &fakeAPI{}, &fakeEC2{})).(driver.Snapshotter); !ok {
		t.Fatal("the ecs driver does not satisfy driver.Snapshotter, so the control " +
			"plane will stop workspaces without preserving anything")
	}
}
