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
cora retry     retry selected reviewers while reusing completed results
cora status    show the latest local run
cora list      list and filter saved runs
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

# Opt into an additional Fable adjudication when the ordinary reviewers
# disagree. This can add substantial provider usage.
cora review --base upstream/main --adjudicate

# Run built-in Go validation in a disposable worktree. Host execution still
# requires an explicit trust decision.
cora review --base upstream/main --profile auto --allow-unsafe-checks

# Retain the completed Codex result and queue only Claude until a recorded
# quota reset time.
cora retry latest --reviewer claude

cora status --active
cora list --state incomplete
cora show latest --verbose
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
queue_timeout = "24h"
require_clean_tree = true
allow_api_billing = false
allow_unsafe_host_checks = false
minimum_approvals = 2
blocking_severities = ["blocker", "major"]

[reviewers.codex]
enabled = true
command = "codex"
model = "gpt-5.6-sol"
effort = "high"
max_concurrency = 2

[reviewers.claude]
enabled = true
command = "claude"
model = "opus"
effort = "high"
max_turns = 50
finalization_turns = 2
# Optional hard ceiling passed to Claude Code; 0 disables it.
max_budget_usd = 0
max_concurrency = 1

[escalation]
enabled = true
model = "fable"
effort = "high"
adjudicate_disagreements = false
security_path_markers = [
  "/.github/workflows/", "/auth/", "/security/", "/crypto/",
  "/iam/", "/permissions/", "/secrets/", "/credentials/", "oauth", "jwt",
]

[[checks]]
name = "unit"
command = ["go", "test", "./..."]
timeout = "10m"
env_allowlist = []

[[validation_profiles]]
name = "go-fast"

[[validation_profiles.checks]]
name = "go-test"
command = ["go", "test", "./..."]
timeout = "15m"
env_allowlist = []
```

By default, CORA refuses API-key authentication. Pass `--allow-api-billing`
only when separately billed usage is intentional.

Configured checks execute code from the reviewed tree. Until sandboxed check
execution is available, CORA refuses to run them unless
`--allow-unsafe-checks` is passed or `allow_unsafe_host_checks = true` is set.
Allowed host checks receive a minimal environment with an ephemeral home and
temporary directory and execute in a disposable detached worktree that is
removed afterward. Add only explicitly required variable names to a check's
`env_allowlist`; this protects the user's checkout and reduces credential
exposure, but it is not a filesystem or network sandbox.

Named validation profiles group checks without forcing every repository to use
one global check list. `--profile auto` currently selects Cora's built-in `go`
profile when the reviewed commit contains `go.mod`; `--profile go` selects it
directly. Trusted base configuration can define additional
`[[validation_profiles]]`. Profile checks retain the same
`--allow-unsafe-checks` requirement.

`minimum_approvals = 2` makes the default policy true two-agent consensus. All
enabled reviewers must complete with full context; a blocking finding or a
failed check requests changes, and an abstention sends the decision to a human.

Claude defaults to Opus at high effort. It reserves its final two turns for a
structured response; if it nevertheless reaches the turn ceiling, CORA saves
a partial abstaining report with incomplete context and omitted paths rather
than discarding all work. `max_budget_usd` can impose an additional Claude Code
cost ceiling.

Changes to reviewer/control files or paths matching
`escalation.security_path_markers` use Fable at high effort instead.
`--security-sensitive` forces the same behavior when path matching is not
sufficient. Disagreement adjudication is disabled by default because it adds
cost and cannot overturn an earlier fail-closed blocking result. Pass
`--adjudicate` or set `adjudicate_disagreements = true` to run a separate
Fable/high adjudication and retain all three reports; adjudication never removes
a finding from an earlier report.

`effort` accepts `low`, `medium`, `high`, `xhigh`, or `max`; Codex also accepts
`none` and `minimal`. Codex defaults to `gpt-5.6-sol` at high effort so its effective
selection and API-equivalent pricing are reproducible even when user CLI
defaults change.

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

Provider concurrency uses a user-global FIFO queue across Cora processes and
repositories. `cora status --active` reports each reviewer's position, number
ahead, and a best-effort ETA derived from recent executions. Queue wait is
recorded separately from execution time and does not consume reviewer or
overall execution timeouts; `queue_timeout` bounds the wait itself. Claude
defaults to one concurrent request and Codex to two; adjust
`max_concurrency` per reviewer when the subscription permits it. Quota failures
are marked retryable and their reset time is saved when the CLI reports one.
`cora retry` creates a child run, reuses completed base reviewers and checks,
and queues only the selected provider. It also recovers reset timestamps from
older saved Claude errors that predate the structured retry field. `--no-wait`
returns immediately when a saved reset time is still in the future.

After every reviewer and at the end of a run, CORA prints the effective model,
effort, turns, thinking tokens, and API-equivalent cost. The normalized values
are also saved per reviewer and in aggregate in `manifest.json` and
`decision.json`. Retry records distinguish incremental usage for that attempt
from cumulative usage across their parent lineage; the compatibility `usage`
field is cumulative. Claude cost comes from the CLI result envelope; Codex cost is
calculated from its reported tokens and the pricing table named by
`cost_source`. A metric is shown as `n/a` when the installed provider CLI does
not expose enough telemetry, and mixed-availability totals are labeled
`partial` rather than silently treated as complete.

An approved run with minor or note findings is displayed as `APPROVED WITH
NON-BLOCKING FINDINGS` while retaining the machine state `approved` and exit
code 0. Provider process failures retain the provider's actual error in the
top-level decision, so JSON automation need not open raw logs to diagnose an
unsupported model or similar failure.
Durations in JSON use integer `duration_ms` and `elapsed_ms` fields instead of
Go's nanosecond representation.

Equivalent findings are consolidated by location and claim similarity for the
decision and human summary while each original reviewer report remains intact.
`cora show --verbose` adds confidence, evidence, suggested fixes, omitted paths,
and residual risks.

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

Each active run updates `heartbeat.json` every 30 seconds and prints elapsed
time to stderr. `cora status --active` lists live runs, and `cora list` supports
state and head-SHA filters. `latest` is resolved by run start time instead of
completion order, so concurrent reviews cannot overwrite its meaning.

Builds embed the Cora source SHA and UTC build time. Manifests record those
values plus a credential-free repository identity such as
`github.com/herikwebb/cora`; repositories without a remote fall back to their
root commit identity.

The current record is local and is not cryptographically signed. Treat Git-ref
publication, signatures, and a GitHub status-check bridge as follow-on work
before using CORA as an organization-wide enforcement boundary.
