# Examples

All commands default to `--dry-run`, so nothing is ever destroyed until you add
`--apply`. Credentials come from the standard AWS chain (env vars, shared config,
SSO, instance profile).

## See what would go, with savings

```shell
aws-ami-cleaner --keep-last 3 --older-than 90d --dry-run
```

Keeps the newest three AMIs per `Name`, and of the rest deletes only those older
than 90 days. In-use images (running instances, launch templates, ASGs) are never
listed for deletion.

## Delete what nobody launches anymore

```shell
aws-ami-cleaner --unused-for 60d --keep-last 2 --dry-run
```

An old image that a scheduled job still launches every week stays; an image
nobody has launched in 60 days goes, as long as it is not one of the two newest
of its `Name`. Images never launched at all count from their creation date.

## Clean a tooling account that shares AMIs

Images shared with other accounts or OUs are kept by default (reason `shared`).
If you deregister shared images on purpose, opt out:

```shell
aws-ami-cleaner --older-than 180d --ignore-sharing --dry-run
```

## Restrict to a team's images

```shell
aws-ami-cleaner --older-than 30d --tags team=payments,env=staging --dry-run
```

## Actually delete (with confirmation)

```shell
aws-ami-cleaner --keep-last 5 --older-than 180d --apply
# Delete 12 AMIs and their snapshots? [y/N]:
```

Add `--yes` to skip the prompt in automation (e.g. a scheduled job).

## Multiple regions and another account

```shell
aws-ami-cleaner \
  --regions us-east-1,eu-west-1 \
  --assume-role arn:aws:iam::222233334444:role/ami-janitor \
  --keep-last 3 --older-than 90d --apply --yes
```

Every region is planned first; the confirmation gate covers the whole run.
