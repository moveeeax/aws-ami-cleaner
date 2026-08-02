# aws-ami-cleaner

[![ci](https://github.com/moveeeax/aws-ami-cleaner/actions/workflows/ci.yml/badge.svg)](https://github.com/moveeeax/aws-ami-cleaner/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.24-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

Deregister stale AMIs and delete their backing EBS snapshots on a retention
policy — **safely**. Every AMI referenced by a running instance, launch template
or Auto Scaling group is untouchable, the tool is dry-run by default, and it
prints exactly what it would delete plus the monthly snapshot spend you'd reclaim.

Forgotten AMIs are quiet money: the image itself is free, but each one pins EBS
snapshots you keep paying for every month. This finds the ones nothing is using
anymore and cleans them up without ever risking an image you still need.

## What it does

- **Retention three ways** — keep the newest N per image name (`--keep-last`),
  keep anything younger than an age (`--older-than 90d`), and/or restrict to a
  tag set (`--tags team=payments`). An image is deleted only when **every**
  enabled rule agrees it's expendable — protection wins ties.
- **Never touches in-use AMIs** — running/pending instances, launch template
  `$Latest`/`$Default` versions, and ASG launch configurations are collected
  first and excluded, no matter their age.
- **Cleans up the snapshots too** — after deregistering an image it deletes the
  EBS snapshots from its block device mapping, so you don't leak orphans.
- **Dry-run with savings** — reports each deletion, total GiB, and estimated
  `$/month` reclaimed (default `$0.05/GiB-mo`, overridable).
- **Multi-region and multi-account** — `--regions a,b`, and `--assume-role` for
  cross-account cleanup. Confirmation prompt unless `--yes`.

## How it works

The core is a pure retention engine (`internal/cleaner`) that takes plain domain
types — AMIs, their snapshots, and a set of in-use IDs — and returns a plan. It
has no dependency on the AWS SDK, so its safety rules are unit-tested
exhaustively without ever touching an account. The SDK lives in exactly one place
(`internal/awssrc`), which maps EC2/Auto Scaling responses into those domain
types and executes the plan's mutations.

One detail worth calling out: **`--keep-last` groups by the AMI `Name`, and an
un-named image is its own group of one.** That means keep-last can never let a
newer image in one lineage "cover for" an older, unrelated one — a subtle way a
naive "keep the N newest overall" would delete something it shouldn't.

## Install

```shell
go install github.com/moveeeax/aws-ami-cleaner@latest
```

## Usage

```shell
aws-ami-cleaner --keep-last 3 --older-than 90d --dry-run
```

```text
[DRY-RUN] region=us-east-1: 1 to delete, 3 kept
  DELETE ami-0af12... web-base-2025-12       2026-01-13  8GiB  ~$0.40/mo
  reclaim: 8GiB  ~$0.40/mo (@ $0.050/GiB-mo)

TOTAL: 1 images, 8GiB, ~$0.40/mo across 1 region(s)
dry-run: no changes made (re-run with --apply to execute)
```

When the plan looks right, swap `--dry-run` for `--apply` (you'll be asked to
confirm; add `--yes` for automation). More recipes in [`examples/`](examples/).

| Flag | Meaning |
| --- | --- |
| `--keep-last N` | keep the newest N images per `Name` |
| `--older-than D` | only delete images older than `D` (`90d`, `2w`, `12h`) |
| `--tags k=v,...` | only consider images matching all tags |
| `--regions a,b` | clean these regions (default: resolved region) |
| `--assume-role ARN` | assume a role first (cross-account) |
| `--snapshot-price F` | `$/GiB-month` for the savings estimate |
| `--apply` / `--yes` | actually delete / skip the prompt |

Credentials use the standard AWS chain (env, shared config, SSO, instance
profile). The IAM identity needs `ec2:DescribeImages`, `ec2:DescribeSnapshots`,
`ec2:DescribeInstances`, `ec2:DescribeLaunchTemplateVersions`,
`autoscaling:DescribeLaunchConfigurations`, and — for `--apply` —
`ec2:DeregisterImage` and `ec2:DeleteSnapshot`.

## Development

```shell
go test ./...      # unit tests, no AWS account needed
go vet ./...
```

## License

MIT — see [LICENSE](LICENSE).
