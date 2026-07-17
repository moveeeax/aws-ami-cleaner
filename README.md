# aws-ami-cleaner

> Reclaim wasted spend from forgotten AMIs and orphaned snapshots.

**Status:** 🚧 In development

## Overview

Deregister stale AMIs and delete their snapshots based on a retention policy.

## Features

- Retention by age, count-per-name, and tag filters
- Safety: skip AMIs referenced by launch templates/ASGs/running instances
- Deletes associated EBS snapshots after deregister
- Dry-run with estimated monthly savings
- Per-region and multi-account (assume-role) support

## Stack

Go + aws-sdk-go-v2 (EC2, Autoscaling).

## Usage

```bash
aws-ami-cleaner --keep-last 3 --older-than 90d --dry-run
```

## License

MIT
