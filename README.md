# CORA

CORA is a local-first **Consensus-Oriented Review and Approval** CLI. It runs
independent Codex and Claude code reviews against the same Git change, stores
an auditable local record, and applies a deterministic approval policy.

The reviewer adapters call the installed `codex` and `claude` CLIs. CORA does
not use provider SDKs or handle provider credentials, which lets the CLIs reuse
their existing subscription-backed authentication.

## Status

This repository contains the first working implementation. The initial command
surface is:

```text
cora review    review a branch, range, commit, or working tree
cora status    show the latest local run
cora show      show a saved run
cora verify    verify that an approval still matches a Git commit
cora completion
```

## Build

```bash
make build
./bin/cora --version
```

By default, `make install` puts the binary in `~/.local/bin`, which is commonly
used for user-installed commands:

```bash
make install
command -v cora
```

Override the destination when needed:

```bash
make install INSTALL_DIR=/path/already/on/PATH
```

## Usage

```bash
# Review the current branch against the configured or detected base
cora review --base upstream/main

# Review one commit
cora review --commit abc123

# Review an explicit range
cora review --range abc123..def456

# Review staged, unstaged, and untracked changes. This mode cannot create a
# final approval attestation.
cora review --uncommitted

# Force the security-sensitive escalation policy when paths do not make the
# risk obvious.
cora review --base upstream/main --security-sensitive

cora status
cora show latest --json
cora verify --head HEAD
```

For a commit or range whose head is not currently checked out, CORA creates a
temporary detached Git worktree at that exact head. Reviewers and local checks
therefore see the requested source tree, and the worktree is removed when the
run finishes.

## Pre-PR loop

The intended loop is branch-local and does not require a GitHub pull request:

```bash
# Ask both reviewers to inspect the committed branch delta.
cora review --base upstream/main

# If the exit code is 2, apply the recorded findings, commit, and run again.
cora show latest
cora review --base upstream/main

# Only create the upstream PR after the exact current HEAD verifies.
cora verify --head HEAD
gh pr create --repo OWNER/UPSTREAM --base main --head YOUR_FORK:YOUR_BRANCH
```

Every iteration gets a new immutable-by-convention run directory; earlier
feedback remains available. Coding agents can use `--json` plus the documented
exit codes as their control interface.

Exit codes are part of the CLI contract:

| Code | Meaning |
| ---: | --- |
| 0 | Approved or command succeeded |
| 2 | Changes requested |
| 3 | Human decision required |
| 4 | Review incomplete |
| 5 | Approval is stale |
| 10 | Configuration, Git, or tool failure |

## Configuration

CORA loads personal defaults from the operating system's user configuration
directory, followed by repository settings from `.cora/config.toml`.
Repository settings and relative reviewer prompts are read from the resolved
base revision, so a reviewed change cannot configure its own reviewers or
checks. Repository values override personal values. Run `cora config path` to
print the exact personal configuration path. Common locations are
`~/.config/cora/config.toml` on Linux and
`~/Library/Application Support/cora/config.toml` on macOS. Defaults are
embedded in the binary. A complete starting file is available at
`examples/config.toml`.

```toml
base = "upstream/main"
reviewer_timeout = "15m"
overall_timeout = "45m"
require_clean_tree = true
allow_api_billing = false
allow_unsafe_host_checks = false
minimum_approvals = 2
blocking_severities = ["blocker", "major"]

[reviewers.codex]
enabled = true
command = "codex"
model = "gpt-5.6"
effort = "high"

[reviewers.claude]
enabled = true
command = "claude"
model = "opus"
effort = "high"
max_turns = 50

[escalation]
enabled = true
model = "fable"
effort = "high"
security_path_markers = [
  "/.github/workflows/", "/auth/", "/security/", "/crypto/",
  "/iam/", "/permissions/", "/secrets/", "/credentials/", "oauth", "jwt",
]

[[checks]]
name = "unit"
command = ["go", "test", "./..."]
timeout = "10m"
env_allowlist = []
```

By default, CORA refuses API-key authentication. Pass `--allow-api-billing`
only when separately billed usage is intentional.

Configured checks execute code from the reviewed tree. Until sandboxed check
execution is available, CORA refuses to run them unless
`--allow-unsafe-checks` is passed or `allow_unsafe_host_checks = true` is set.
Allowed host checks receive a minimal environment with an ephemeral home and
temporary directory. Add only explicitly required variable names to a check's
`env_allowlist`; this reduces credential exposure but is not a filesystem or
network sandbox.

`minimum_approvals = 2` makes the default policy true two-agent consensus. All
enabled reviewers must complete with full context; a blocking finding or a
failed check requests changes, and an abstention sends the decision to a human.

Claude defaults to Opus at high effort. Changes to reviewer/control files or
paths matching `escalation.security_path_markers` use Fable at high effort
instead. `--security-sensitive` forces the same behavior when path matching is
not sufficient. When ordinary completed reviews disagree, CORA runs a separate
Fable/high adjudication and retains all three reports; escalation is fail-closed
and never removes a finding from an earlier report.

`effort` accepts `low`, `medium`, `high`, `xhigh`, or `max`; Codex also accepts
`none` and `minimal`. Codex defaults to GPT-5.6 at high effort so its effective selection
and API-equivalent pricing are reproducible even when user CLI defaults change.

The policy fails closed. A timeout, malformed response, missing reviewer,
incomplete context, or interrupted check produces `incomplete`, never an
approval. Reviewer processes run in parallel and are killed as process groups
at their deadlines. If reviewers conflict, any blocking finding or explicit
`request_changes` wins; otherwise an abstention requires human adjudication.

For deterministic, subscription-first execution, the Codex adapter requires a
ChatGPT login, ignores user CLI configuration, and forces a read-only sandbox.
On macOS, CORA also discovers the Codex CLI bundled with ChatGPT when `codex`
is not otherwise available on `PATH`.
The Claude adapter requires first-party Claude.ai subscription authentication
and runs in safe plan mode with read-only tools. Common API-key environment
variables are removed unless `--allow-api-billing` is explicitly passed.

After every reviewer and at the end of a run, CORA prints the effective model,
effort, turns, thinking tokens, and API-equivalent cost. The normalized values
are also saved per reviewer and in aggregate in `manifest.json` and
`decision.json`. Claude cost comes from the CLI result envelope; Codex cost is
calculated from its reported tokens and the pricing table named by
`cost_source`. A metric is shown as `n/a` when the installed provider CLI does
not expose enough telemetry, and mixed-availability totals are labeled
`partial` rather than silently treated as complete.

## Records

Run records are stored beneath the repository's Git common directory:

```text
.git/cora/runs/<run-id>/
```

This keeps local records shared by Git worktrees without adding review output
to the source tree. Each record includes the canonical patch, exact prompt and
schema, raw tool logs, normalized reviewer reports, check logs, manifest, event
stream, and deterministic decision. Publishing signed records to a dedicated
Git ref is planned as a separate command.

The current record is local and is not cryptographically signed. Treat Git-ref
publication, signatures, and a GitHub status-check bridge as follow-on work
before using CORA as an organization-wide enforcement boundary.
