package awssrc

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeEC2 returns canned responses. Single-page outputs are enough to exercise
// the mapping and the in-use collection; the paginators handle one page here.
type fakeEC2 struct {
	images       []ec2types.Image
	snapshots    []ec2types.Snapshot
	reservations []ec2types.Reservation
	ltVersions   []ec2types.LaunchTemplateVersion

	deregistered []string
	deletedSnaps []string
}

func (f *fakeEC2) DescribeImages(context.Context, *ec2.DescribeImagesInput, ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	return &ec2.DescribeImagesOutput{Images: f.images}, nil
}
func (f *fakeEC2) DescribeSnapshots(_ context.Context, in *ec2.DescribeSnapshotsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
	return &ec2.DescribeSnapshotsOutput{Snapshots: f.snapshots}, nil
}
func (f *fakeEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: f.reservations}, nil
}
func (f *fakeEC2) DescribeLaunchTemplateVersions(context.Context, *ec2.DescribeLaunchTemplateVersionsInput, ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplateVersionsOutput, error) {
	return &ec2.DescribeLaunchTemplateVersionsOutput{LaunchTemplateVersions: f.ltVersions}, nil
}
func (f *fakeEC2) DeregisterImage(_ context.Context, in *ec2.DeregisterImageInput, _ ...func(*ec2.Options)) (*ec2.DeregisterImageOutput, error) {
	f.deregistered = append(f.deregistered, aws.ToString(in.ImageId))
	return &ec2.DeregisterImageOutput{}, nil
}
func (f *fakeEC2) DeleteSnapshot(_ context.Context, in *ec2.DeleteSnapshotInput, _ ...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error) {
	f.deletedSnaps = append(f.deletedSnaps, aws.ToString(in.SnapshotId))
	return &ec2.DeleteSnapshotOutput{}, nil
}

type fakeASG struct{ lcs []astypes.LaunchConfiguration }

func (f *fakeASG) DescribeAutoScalingGroups(context.Context, *autoscaling.DescribeAutoScalingGroupsInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	return &autoscaling.DescribeAutoScalingGroupsOutput{}, nil
}
func (f *fakeASG) DescribeLaunchConfigurations(context.Context, *autoscaling.DescribeLaunchConfigurationsInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeLaunchConfigurationsOutput, error) {
	return &autoscaling.DescribeLaunchConfigurationsOutput{LaunchConfigurations: f.lcs}, nil
}

func TestOwnedImagesJoinsSnapshotSizesAndTags(t *testing.T) {
	f := &fakeEC2{
		images: []ec2types.Image{{
			ImageId:      aws.String("ami-1"),
			Name:         aws.String("web"),
			CreationDate: aws.String("2026-01-02T03:04:05.000Z"),
			Tags:         []ec2types.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
			BlockDeviceMappings: []ec2types.BlockDeviceMapping{
				{Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String("snap-a")}},
				{Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String("snap-b")}},
			},
		}},
		snapshots: []ec2types.Snapshot{
			{SnapshotId: aws.String("snap-a"), VolumeSize: aws.Int32(8)},
			{SnapshotId: aws.String("snap-b"), VolumeSize: aws.Int32(30)},
		},
	}
	src := &Source{EC2: f}
	amis, err := src.OwnedImages(context.Background())
	if err != nil {
		t.Fatalf("OwnedImages: %v", err)
	}
	if len(amis) != 1 {
		t.Fatalf("want 1 AMI, got %d", len(amis))
	}
	a := amis[0]
	if a.Tags["env"] != "dev" {
		t.Errorf("tag not mapped: %v", a.Tags)
	}
	if len(a.Snapshots) != 2 {
		t.Fatalf("want 2 snapshots, got %d", len(a.Snapshots))
	}
	var total int64
	for _, s := range a.Snapshots {
		total += s.SizeGiB
	}
	if total != 38 {
		t.Errorf("snapshot sizes not joined: total=%d want 38", total)
	}
	if a.Created.Year() != 2026 || a.Created.Month() != 1 {
		t.Errorf("creation time not parsed: %v", a.Created)
	}
}

func TestInUseCollectsAllSourcesAndSkipsTerminated(t *testing.T) {
	f := &fakeEC2{
		reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
			{ImageId: aws.String("ami-running"), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
			{ImageId: aws.String("ami-terminated"), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameTerminated}},
		}}},
		ltVersions: []ec2types.LaunchTemplateVersion{
			{LaunchTemplateData: &ec2types.ResponseLaunchTemplateData{ImageId: aws.String("ami-lt")}},
		},
	}
	asg := &fakeASG{lcs: []astypes.LaunchConfiguration{{ImageId: aws.String("ami-lc")}}}
	src := &Source{EC2: f, ASG: asg}

	inUse, err := src.InUseImageIDs(context.Background())
	if err != nil {
		t.Fatalf("InUseImageIDs: %v", err)
	}
	for _, want := range []string{"ami-running", "ami-lt", "ami-lc"} {
		if _, ok := inUse[want]; !ok {
			t.Errorf("expected %s in-use, set=%v", want, keys(inUse))
		}
	}
	if _, ok := inUse["ami-terminated"]; ok {
		t.Errorf("terminated instance's AMI wrongly marked in-use")
	}
}

func TestDeleterCallsThrough(t *testing.T) {
	f := &fakeEC2{}
	src := &Source{EC2: f}
	if err := src.DeregisterImage(context.Background(), "ami-9"); err != nil {
		t.Fatal(err)
	}
	if err := src.DeleteSnapshot(context.Background(), "snap-9"); err != nil {
		t.Fatal(err)
	}
	if len(f.deregistered) != 1 || f.deregistered[0] != "ami-9" {
		t.Errorf("deregister not forwarded: %v", f.deregistered)
	}
	if len(f.deletedSnaps) != 1 || f.deletedSnaps[0] != "snap-9" {
		t.Errorf("delete snapshot not forwarded: %v", f.deletedSnaps)
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
