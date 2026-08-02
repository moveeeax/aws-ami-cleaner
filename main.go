// Command aws-ami-cleaner deregisters stale AMIs and deletes their backing EBS
// snapshots according to a retention policy — safely, with a dry-run default and
// an explicit confirmation gate before anything is destroyed.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/moveeeax/aws-ami-cleaner/internal/awssrc"
	"github.com/moveeeax/aws-ami-cleaner/internal/cleaner"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "aws-ami-cleaner: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string, stdin *os.File, stdout *os.File) error {
	fs := flag.NewFlagSet("aws-ami-cleaner", flag.ContinueOnError)
	var (
		regions   = fs.String("regions", "", "comma-separated regions (default: the SDK's resolved region)")
		keepLast  = fs.Int("keep-last", 0, "keep the newest N images per Name (0 = disabled)")
		olderThan = fs.String("older-than", "", "only delete images older than this, e.g. 90d, 2w, 12h (0 = disabled)")
		tagsFlag  = fs.String("tags", "", "only consider images matching all key=value tags, comma-separated")
		roleARN   = fs.String("assume-role", "", "IAM role ARN to assume before cleaning (multi-account)")
		price     = fs.Float64("snapshot-price", cleaner.DefaultSnapshotPriceUSD, "EBS snapshot price in $/GiB-month for savings estimate")
		dryRun    = fs.Bool("dry-run", true, "report only; make no changes")
		apply     = fs.Bool("apply", false, "actually deregister and delete (overrides --dry-run)")
		yes       = fs.Bool("yes", false, "skip the confirmation prompt")
	)
	fs.SetOutput(stdout)
	if err := fs.Parse(args); err != nil {
		return err
	}

	olderDur, err := cleaner.ParseAge(*olderThan)
	if err != nil {
		return err
	}
	tagFilter, err := parseTags(*tagsFlag)
	if err != nil {
		return err
	}
	policy := cleaner.Policy{KeepLast: *keepLast, OlderThan: olderDur, TagFilter: tagFilter}

	if *keepLast <= 0 && olderDur == 0 && len(tagFilter) == 0 {
		return fmt.Errorf("refusing to run with no retention rules: set at least one of --keep-last, --older-than, --tags")
	}

	// --apply flips off the safety default; dry-run stays true otherwise.
	effectiveDryRun := *dryRun && !*apply

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	if *roleARN != "" {
		stsClient := sts.NewFromConfig(cfg)
		cfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(stsClient, *roleARN))
	}

	regionList := splitRegions(*regions)
	if len(regionList) == 0 {
		if cfg.Region == "" {
			return fmt.Errorf("no region: pass --regions or set AWS_REGION")
		}
		regionList = []string{cfg.Region}
	}

	// Plan every region first, then confirm once, then apply. This keeps the
	// destructive gate in front of the whole operation, not per region.
	type regionPlan struct {
		region string
		src    *awssrc.Source
		report *cleaner.Report
	}
	var plans []regionPlan
	var grandGiB int64
	var grandUSD float64

	for _, region := range regionList {
		rc := cfg.Copy()
		rc.Region = region
		src := &awssrc.Source{
			EC2: ec2.NewFromConfig(rc),
			ASG: autoscaling.NewFromConfig(rc),
		}
		amis, err := src.OwnedImages(ctx)
		if err != nil {
			return fmt.Errorf("region %s: %w", region, err)
		}
		inUse, err := src.InUseImageIDs(ctx)
		if err != nil {
			return fmt.Errorf("region %s: %w", region, err)
		}
		rep := cleaner.Plan(amis, inUse, policy, nowUTC(), *price)
		fmt.Fprint(stdout, rep.Summary(region, effectiveDryRun))
		plans = append(plans, regionPlan{region: region, src: src, report: rep})
		grandGiB += rep.TotalGiB
		grandUSD += rep.TotalSavingsUSD
	}

	total := 0
	for _, p := range plans {
		total += len(p.report.Delete)
	}
	fmt.Fprintf(stdout, "\nTOTAL: %d images, %dGiB, ~$%.2f/mo across %d region(s)\n", total, grandGiB, grandUSD, len(regionList))

	if effectiveDryRun {
		fmt.Fprintln(stdout, "dry-run: no changes made (re-run with --apply to execute)")
		return nil
	}
	if total == 0 {
		fmt.Fprintln(stdout, "nothing to delete.")
		return nil
	}
	if !*yes {
		ok, err := confirm(stdin, stdout, fmt.Sprintf("Delete %d AMIs and their snapshots?", total))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(stdout, "aborted.")
			return nil
		}
	}

	for _, p := range plans {
		if err := cleaner.Apply(ctx, p.src, p.report, false); err != nil {
			return fmt.Errorf("region %s: %w", p.region, err)
		}
		fmt.Fprintf(stdout, "region %s: deleted %d images\n", p.region, len(p.report.Delete))
	}
	return nil
}

// parseTags turns "env=dev,team=core" into a map. An empty string yields nil.
func parseTags(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("invalid --tags entry %q: want key=value", pair)
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m, nil
}

func splitRegions(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

func nowUTC() time.Time { return time.Now().UTC() }

func confirm(stdin *os.File, stdout *os.File, prompt string) (bool, error) {
	fmt.Fprintf(stdout, "%s [y/N]: ", prompt)
	sc := bufio.NewScanner(stdin)
	if !sc.Scan() {
		return false, sc.Err()
	}
	ans := strings.ToLower(strings.TrimSpace(sc.Text()))
	return ans == "y" || ans == "yes", nil
}
