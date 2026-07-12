---
title: Registry commands
description: "The registry command family: store, list, and remove the credentials the daemon uses to pull private images."
---

# Registry commands

Store per-registry credentials the daemon uses to pull **private images** — for `run`, `app create`, and an app's re-pull on restart. Credentials live on the daemon (not read from your local `~/.docker/config.json`), so a durable app on a private image survives a restart. See [Private registries](../registry.md) for the full picture and the supported-registry table.

| Command | Description |
|---|---|
| `registry login <host> [flags]` | store (or replace) a credential for a registry host |
| `registry ls` | list stored credentials — **host + username only, never the secret** |
| `registry logout <host>` | remove a stored credential |

## Login flags

`-u/--username` (may be empty for token-only registries), and one of: `--password-stdin` (read the secret from stdin — recommended, keeps it out of shell history), `-p/--password <secret>` (warns; ends up in history), or — with neither — an interactive **masked** prompt.

```bash
# Recommended: pipe the token in
echo "$GH_TOKEN" | crucible registry login ghcr.io -u my-user --password-stdin

# AWS ECR (12h token — re-run periodically)
aws ecr get-login-password --region us-east-1 \
  | crucible registry login <acct>.dkr.ecr.us-east-1.amazonaws.com -u AWS --password-stdin

crucible registry ls          # HOST  USERNAME  ADDED  (no secret)
crucible registry logout ghcr.io
```

> [!NOTE]
> The secret is sent to the daemon and never printed back by any command. Managing credentials requires a token with the `registry` [policy](../policy.md) operation; `registry ls` needs only `read`.
