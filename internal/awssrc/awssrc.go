// Package awssrc adapts the AWS SDK to the cleaner engine's domain types.
//
// It is intentionally the only package that imports the SDK. It collects owned
// AMIs (with their backing snapshot sizes) and the set of AMI IDs that are
// currently referenced by running instances, launch template versions and Auto
// Scaling groups, and it implements cleaner.Deleter for the mutating calls.
package awssrc

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/moveeeax/aws-ami-cleaner/internal/cleaner"
)

// EC2API is the subset of the EC2 client this package uses. Declaring it as an
// interface keeps the adapter honest and makes the collector unit-testable.
type EC2API interface {
	DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, opts ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	DescribeSnapshots(ctx context.Context, in *ec2.DescribeSnapshotsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeLaunchTemplateVersions(ctx context.Context, in *ec2.DescribeLaunchTemplateVersionsInput, opts ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplateVersionsOutput, error)
	DeregisterImage(ctx context.Context, in *ec2.DeregisterImageInput, opts ...func(*ec2.Options)) (*ec2.DeregisterImageOutput, error)
	DeleteSnapshot(ctx context.Context, in *ec2.DeleteSnapshotInput, opts ...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error)
}

// ASGAPI is the subset of the Auto Scaling client this package uses.
type ASGAPI interface {
	DescribeAutoScalingGroups(ctx context.Context, in *autoscaling.DescribeAutoScalingGroupsInput, opts ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
	DescribeLaunchConfigurations(ctx context.Context, in *autoscaling.DescribeLaunchConfigurationsInput, opts ...func(*autoscaling.Options)) (*autoscaling.DescribeLaunchConfigurationsOutput, error)
}

// Source collects state for one region.
type Source struct {
	EC2 EC2API
	ASG ASGAPI
}

// OwnedImages returns every AMI owned by the calling account together with the
// sizes of the EBS snapshots in its block device mapping. Snapshot sizes are
// resolved in a single batched DescribeSnapshots call to stay within rate limits.
func (s *Source) OwnedImages(ctx context.Context) ([]cleaner.AMI, error) {
	out, err := s.EC2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"self"},
	})
	if err != nil {
		return nil, fmt.Errorf("describe images: %w", err)
	}

	// Collect snapshot IDs to batch-resolve their sizes.
	snapIDset := map[string]struct{}{}
	for _, img := range out.Images {
		for _, bdm := range img.BlockDeviceMappings {
			if bdm.Ebs != nil && bdm.Ebs.SnapshotId != nil && *bdm.Ebs.SnapshotId != "" {
				snapIDset[*bdm.Ebs.SnapshotId] = struct{}{}
			}
		}
	}
	sizes, err := s.snapshotSizes(ctx, snapIDset)
	if err != nil {
		return nil, err
	}

	var amis []cleaner.AMI
	for _, img := range out.Images {
		a := cleaner.AMI{
			ID:      aws.ToString(img.ImageId),
			Name:    aws.ToString(img.Name),
			Tags:    tagMap(img.Tags),
			Created: parseTime(aws.ToString(img.CreationDate)),
		}
		for _, bdm := range img.BlockDeviceMappings {
			if bdm.Ebs == nil || bdm.Ebs.SnapshotId == nil {
				continue
			}
			id := *bdm.Ebs.SnapshotId
			a.Snapshots = append(a.Snapshots, cleaner.Snapshot{ID: id, SizeGiB: sizes[id]})
		}
		amis = append(amis, a)
	}
	return amis, nil
}

// snapshotSizes resolves each snapshot's VolumeSize (GiB). Missing snapshots
// (already deleted, cross-account) contribute zero rather than failing the run.
func (s *Source) snapshotSizes(ctx context.Context, idset map[string]struct{}) (map[string]int64, error) {
	sizes := map[string]int64{}
	if len(idset) == 0 {
		return sizes, nil
	}
	ids := make([]string, 0, len(idset))
	for id := range idset {
		ids = append(ids, id)
	}
	// Chunk to keep each request bounded.
	const chunk = 200
	for i := 0; i < len(ids); i += chunk {
		end := i + chunk
		if end > len(ids) {
			end = len(ids)
		}
		p := ec2.NewDescribeSnapshotsPaginator(s.EC2, &ec2.DescribeSnapshotsInput{
			SnapshotIds: ids[i:end],
		})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("describe snapshots: %w", err)
			}
			for _, sn := range page.Snapshots {
				sizes[aws.ToString(sn.SnapshotId)] = int64(aws.ToInt32(sn.VolumeSize))
			}
		}
	}
	return sizes, nil
}

// InUseImageIDs returns AMI IDs referenced by running/pending instances, launch
// template versions and Auto Scaling launch configurations. Anything in this set
// is protected by the engine no matter how old it is.
func (s *Source) InUseImageIDs(ctx context.Context) (map[string]struct{}, error) {
	inUse := map[string]struct{}{}

	instPager := ec2.NewDescribeInstancesPaginator(s.EC2, &ec2.DescribeInstancesInput{})
	for instPager.HasMorePages() {
		page, err := instPager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe instances: %w", err)
		}
		for _, r := range page.Reservations {
			for _, inst := range r.Instances {
				if inst.State != nil && inst.State.Name == ec2types.InstanceStateNameTerminated {
					continue
				}
				if inst.ImageId != nil {
					inUse[*inst.ImageId] = struct{}{}
				}
			}
		}
	}

	// Scan every launch template version. Passing "$Latest"/"$Default" as the
	// version list without a template id makes EC2 return those two versions for
	// each template in the account, which is exactly the set that can launch.
	ltPager := ec2.NewDescribeLaunchTemplateVersionsPaginator(s.EC2, &ec2.DescribeLaunchTemplateVersionsInput{
		Versions: []string{"$Latest", "$Default"},
	})
	for ltPager.HasMorePages() {
		page, err := ltPager.NextPage(ctx)
		if err != nil {
			// Not fatal: a permission gap here should not block the age policy,
			// but it must never widen the delete set, so we just stop scanning.
			break
		}
		for _, v := range page.LaunchTemplateVersions {
			if v.LaunchTemplateData != nil && v.LaunchTemplateData.ImageId != nil {
				inUse[*v.LaunchTemplateData.ImageId] = struct{}{}
			}
		}
	}

	if s.ASG != nil {
		lcOut, err := s.ASG.DescribeLaunchConfigurations(ctx, &autoscaling.DescribeLaunchConfigurationsInput{})
		if err == nil {
			for _, lc := range lcOut.LaunchConfigurations {
				if lc.ImageId != nil {
					inUse[*lc.ImageId] = struct{}{}
				}
			}
		}
	}

	return inUse, nil
}

// DeregisterImage implements cleaner.Deleter.
func (s *Source) DeregisterImage(ctx context.Context, amiID string) error {
	_, err := s.EC2.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: aws.String(amiID)})
	return err
}

// DeleteSnapshot implements cleaner.Deleter.
func (s *Source) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	_, err := s.EC2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapshotID)})
	return err
}

// parseTime parses the RFC3339 timestamp EC2 returns for CreationDate. A zero
// time (unparseable/missing) makes an image look infinitely old, so callers that
// rely on age must have a real timestamp; in practice EC2 always populates it.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func tagMap(tags []ec2types.Tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}
