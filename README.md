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
cora plan      preview the effective target, policy, reviewers, and checks
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

# Preview the same trusted-base policy without creating a run or invoking a
# provider. Add --json for automation.
cora plan --base upstream/main --strict --profile auto --allow-unsafe-checks

# Review one commit
cora review --commit abc123

# Review an explicit range
cora review --range abc123..def456

# Review staged, unstaged, and untracked changes. This mode cannot create a
# final approval attestation.
cora review --uncommitted

# Add a targeted Fable/high security pass when paths do not make the risk
# obvious. The ordinary Opus/high review still runs.
cora review --base upstream/main --security-sensitive

# Opt into an additional Fable adjudication when the ordinary reviewers
# disagree. This can add substantial provider usage.
cora review --base upstream/main --adjudicate

# Treat minor findings as blocking and require at least one validation check.
cora review --base upstream/main --strict --profile auto --allow-unsafe-checks

# Opt into a bounded coding-agent loop. Cora reuses only a policy-compatible
# exact approval, reviews each fix delta, then requires a final full review.
cora review --base upstream/main --auto-fix --until minor --max-iterations 5

# Continue the same parent loop after a retryable provider quota reset.
cora review --auto-fix --resume <loop-id>

# Run built-in Go validation in a disposable clone. Host execution still
# requires an explicit trust decision.
cora review --base upstream/main --profile auto --allow-unsafe-checks

# Import an independently produced passing test attestation for this exact
# repository/base/head/diff. CORA records it but does not run its command.
cora review --base upstream/main --validation-evidence /path/to/ci-evidence.json

# Capture bounded official documentation or advisory pages once, then give the
# same immutable snapshot to every offline reviewer.
cora review --base upstream/main --allow-review-web \
  --web-evidence https://pkg.go.dev/net/http \
  --web-evidence https://go.dev/security/vuln/

# Retain the completed Codex result and queue only Claude until a recorded
# quota reset time.
cora retry latest --reviewer claude

# Raise only the limits that ended the prior attempt. The child run records
# these overrides while preserving the parent's model, effort, and evidence.
cora retry latest --reviewer claude --reviewer-timeout 30m --overall-timeout 1h --max-turns 65

cora status --active
cora list --state incomplete
cora show latest --verbose
cora show latest --json
cora verify --head HEAD
```

`cora plan` accepts the review targeting and policy flags (`--base`,
`--commit`, `--range`, `--uncommitted`, `--parent`, `--profile`, `--strict`,
`--security-sensitive`, `--adjudicate`, and the billing/check/web authorization
flags). It also accepts repeatable `--validation-evidence` files and
`--web-evidence` HTTPS URLs. Planning validates these inputs without importing
or fetching them. It
resolves repository configuration from the trusted base revision,
then reports the exact target and diff hash, effective ordinary and conditional
reviewer model/effort/limits, security triggers and matched paths, expanded
validation checks, and provider concurrency demand. Planning is read-only: it
does not create a run, acquire a provider slot, execute a check, or invoke a
reviewer. Capacity output therefore distinguishes configured global limits and
planned demand from live availability, which remains unknown until review-time
slot acquisition.

Each reviewer and local-check phase receives an independent local clone at the
exact target. Cora removes every remote before execution and discards the clone
afterward, so generated files and disposable Git changes cannot affect the
user's checkout or repository refs.

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

## Auto-fix loop

`--auto-fix` is opt-in and operates only on a clean, checked-out feature branch.
After each review, Cora sends consolidated findings at or above `--until` to a
separately configured Codex coding agent running in `workspace-write` mode. The
agent may edit the current working tree, but Cora instructs it not to commit,
change branches or Git refs, use the network, push, or open a pull request. Cora
also verifies that `HEAD` did not move before continuing.

When the initial exact diff already has an approval produced under the same
effective review policy, Cora keeps that approval as an immutable baseline and
reviews the cumulative coding-agent delta instead of paying to rediscover the
unchanged code. Baseline compatibility includes strictness, reviewer quorum and
settings, required security review, and the exact validation checks. A weaker
or differently configured approval is never reused. Web-backed approvals are
also ineligible because auto-fix does not replay web evidence into its review
lineage. Every review uses an exact snapshot, and after the delta is approved
Cora always performs a fresh full review of the complete working tree against
the original merge base. That final review includes the branch's committed
changes, agent edits, and untracked files. Checks run in disposable materialized
clones. Approval requires the ordinary Cora policy, all configured reviewers to
return `approve`, every required check to pass, and no open finding at or above
the selected threshold. An adjudicated disagreement is therefore insufficient
for auto-fix approval.

The loop stops fail-closed on incomplete reviews, abstentions, failed checks,
agent failures, repeated equivalent findings, unchanged patches, Git-state
changes, or any configured limit. `--until` accepts `blocker`, `major`, or
`minor`; it controls which findings the agent attempts and never weakens the
normal blocking policy. CLI flags can override the trusted-base `[auto_fix]`
defaults with `--max-iterations`, `--max-duration`, `--max-turns`,
`--max-cost-usd`, and `--agent-timeout`.

A provider quota failure with a known retry time pauses the parent loop instead
of discarding it. Cora preserves completed ordinary, security, adjudication,
cross-examination, and check results by exact-diff lineage and exits with code
6. Resume it with `cora review --auto-fix --resume <loop-id>` after the reported
reset. The original effective policy, checks, security classification, reviewer
settings, and loop limits are recorded with the parent and restored on resume;
they cannot silently weaken because configuration changed while the loop was
paused. Paused loops remain visible in `cora status --active`.

Cora never commits or reverts the agent's edits. Successful and partial edits
remain in the feature-branch working tree for inspection, correction, and an
explicit user-created commit.

Review and coding-agent turns plus API-equivalent cost are accumulated across
the whole loop. Provider CLIs expose final usage only after a process exits, so
a single in-flight step can cross a turn or cost ceiling; Cora records that step
and refuses to continue or approve. If the provider does not expose the usage
needed to enforce a configured ceiling, the loop stops incomplete.

Exit codes are part of the CLI contract:

| Code | Meaning |
| ---: | --- |
| 0 | Approved or command succeeded |
| 2 | Changes requested |
| 3 | Human decision required |
| 4 | Review incomplete |
| 5 | Approval is stale |
| 6 | Auto-fix paused for a retryable quota reset |
| 10 | Configuration, Git, or tool failure |
| 130 | Canceled by an interrupt or termination signal |

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
strict = false
cross_examine_blocking_findings = true
require_clean_tree = true
allow_api_billing = false
allow_review_web = false
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
# Cora hard-caps tool-enabled inspection at max_turns-finalization_turns,
# then uses the reserve in a separate tools-disabled finalizer if necessary.
finalization_turns = 2
# Optional hard ceiling passed to Claude Code; 0 disables it.
max_budget_usd = 0
max_concurrency = 1

[escalation]
enabled = true
model = "fable"
effort = "high"
# Omit either override to inherit it from [reviewers.claude].
# max_turns = 40
# max_budget_usd = 6
adjudicate_disagreements = false
security_path_markers = [
  "/.github/workflows/", "/auth/", "/security/", "/crypto/",
  "/iam/", "/permissions/", "/secrets/", "/credentials/", "oauth", "jwt",
]

[cross_examination]
timeout = "10m"
max_turns = 20
# Optional hard ceiling passed to Claude Code; 0 disables it.
max_budget_usd = 5

[auto_fix]
command = "codex"
model = "gpt-5.6-sol"
effort = "high"
until = "major"
agent_timeout = "20m"
max_duration = "1h"
max_iterations = 5
max_turns = 250
max_cost_usd = 50
max_concurrency = 1

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

Reviewer web evidence is also default-off. Pass `--allow-review-web` (or set
`allow_review_web = true` in trusted configuration) together with one or more
repeatable `--web-evidence URL` values. Each URL is an exact operator-selected
source; Cora does not discover or follow URLs from the reviewed repository.
Only absolute HTTPS URLs on public DNS addresses and the default HTTPS port are
accepted. Credentials, query strings, and fragments are rejected, redirects
must remain on the same explicitly selected origin, environment proxies are
ignored, and local, private, link-local, metadata, documentation, and other
non-public IP ranges are blocked at dial time. Cora sends an unauthenticated GET
with no cookie jar.

Cora captures at most four sources, retaining at most 32 KiB of UTF-8 textual
content per source and 64 KiB across the review. Larger individual responses are
visibly marked as truncated; capture fails if the aggregate presentation would
exceed its bound. Cora saves the exact bounded bodies, a strict metadata index,
and the exact prompt appendix beneath `web-evidence/` in the private run record.
`manifest.json` records the individual SHA-256 hashes and a composite
`snapshot_sha256` that binds the index and rendered appendix. The appendix is
JSON-escaped and labels every page as untrusted corroborating material. All
ordinary, security, adjudication, and cross-examination reviewers receive the
same bytes.

A retry validates and copies that exact snapshot into its child record without
refetching. It reruns the whole review, including any conditional role newly
triggered by changed ordinary reports, so `--reviewer` is rejected for these
retries. Changing or refreshing the evidence requires a fresh review. Web
evidence never counts as a passing validation check, cannot be combined with
`--auto-fix`, and a web-backed approval cannot seed an auto-fix baseline.

This option does not enable provider-native WebFetch, WebSearch, MCP, or shell
networking. Codex, Claude/Fable, reviewer-selected tests, and Bash remain under
their existing network-denied sandboxes. Web-backed records use a versioned run
collection that older Cora binaries do not enumerate. They also carry an
existing delta-scope compatibility sentinel that delta-aware older readers
already reject for retry, verification, and approval-baseline reuse. Upgrade
Cora to inspect or retry these records; current readers validate the guard and
present their effective review scope as `full`.

Configured checks execute code from the reviewed tree. Until sandboxed check
execution is available, CORA refuses to run them unless
`--allow-unsafe-checks` is passed or `allow_unsafe_host_checks = true` is set.
Allowed host checks receive a minimal environment with an ephemeral home and
temporary directory and execute in a disposable remote-free clone that is
removed afterward. Add only explicitly required variable names to a check's
`env_allowlist`; this protects the user's checkout and reduces credential
exposure, but it is not a filesystem or network sandbox.

Named validation profiles group checks without forcing every repository to use
one global check list. `--profile auto` selects Cora's built-in `go`, `node`,
and `python` profiles from `go.mod`, `package.json`, and common Python project
markers; explicit `--profile go`, `--profile node`, and `--profile python`
selection is also available. Trusted base configuration can define additional
`[[validation_profiles]]`. Profile checks retain the same
`--allow-unsafe-checks` requirement.

When trusted host checks are enabled but no profile was selected, CORA performs
the same auto-detection automatically. Without a configured validation check or
imported passing attestation, CORA records `validation_status = "not_run"` plus
a residual risk instead of implying that reviewer-selected tests provide
deterministic validation. Strict policy fails closed as `incomplete` when no
validation profile, configured check, or imported passing evidence is
available.

An external CI or test operator can instead provide a passing attestation with
the repeatable `--validation-evidence PATH` flag. Use `cora plan --json` to get
`repository_identity` and the exact `target.base_sha`, `target.head_sha`, and
`target.diff_hash`, then produce a JSON file like this:

```json
{
  "schema_version": "1",
  "name": "ci-unit",
  "repository_identity": "github.com/example/project",
  "base_sha": "0123456789abcdef0123456789abcdef01234567",
  "head_sha": "89abcdef0123456789abcdef0123456789abcdef",
  "diff_hash": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "status": "passed",
  "verified_at": "2026-09-03T14:30:00Z",
  "verifier": "github-actions",
  "source": "https://github.com/example/project/actions/runs/123",
  "command": ["go", "test", "./..."],
  "summary": "All unit tests passed."
}
```

CORA requires `status = "passed"`, rejects unknown or duplicate fields and any
repository/base/head/diff mismatch, and treats the imported result as a normal
validation check. It copies the exact source bytes into the private run record
with a SHA-256 hash and repeats that validation before retry, verification, or
approval-lineage reuse. The `verifier` and `source` fields are
operator-supplied labels, not cryptographic authentication; importing the file
is an explicit trust decision. CORA records `command` for audit purposes and
never executes it. Because the attestation is bound to one exact diff,
`--validation-evidence` cannot be combined with `--auto-fix`.

`minimum_approvals = 2` makes the default policy true two-agent consensus. All
enabled reviewers must complete with full context; a corroborated blocking
finding or a failed check requests changes, and an abstention sends the
decision to a human.
Set `strict = true` or pass `--strict` to add `minor` to the blocking severities
and require at least one validation check. Notes remain non-blocking.

Claude defaults to Opus at high effort. Cora mechanically reserves its final
turns for the tools-disabled finalization phase described below. Both reviewers
receive a low-overhead recovery contract:
after confirming a finding, they checkpoint only when the confirmed evidence
changes. If a reviewer times out, or Claude reaches its turn ceiling, CORA
retains any valid provider output or checkpoint as a partial abstaining report
with incomplete context. Partial evidence remains visible but can never approve
the run. Recovery files use a randomized private directory separate from the
temporary and cache directory inherited by repository test processes, and a
partial-only finding is not promoted into later carried findings.
`max_budget_usd` can impose an additional Claude Code cost ceiling.

Changes to reviewer/control files or paths matching
`escalation.security_path_markers` add a targeted Fable/high security pass;
they do not replace the ordinary full-diff Opus/high review. The focused pass
reviews each sensitive path plus only the transitive callers, callees, trust
boundaries, configuration, and deployment flow needed to establish concrete
security reachability. Its result, effective model/effort, usage, prompt hash,
and prompt are recorded separately under `security_reviews` and
`security-review.prompt.md`.
Optional `escalation.max_turns` and `escalation.max_budget_usd` values override
the corresponding Claude reviewer ceilings for targeted security and broad
adjudication passes. Omit an override to inherit the ordinary Claude
value; set `max_budget_usd = 0` explicitly to remove an inherited cost ceiling.
`--security-sensitive` forces the same behavior when path matching is not
sufficient. This extra pass is required and fails closed on incomplete context,
abstention, or a blocking finding, but its scoped approval does not count toward
the ordinary `minimum_approvals` quorum. A finding from this already-targeted
Fable pass blocks directly and does not trigger a redundant Fable
cross-examination merely because it has one source. Cora defers a Fable pass
when an independent completed result already fixes the outcome and the pass
cannot change it; a potentially disprovable uncorroborated major still receives
the targeted reachability adjudication it needs.

Independently, `cross_examine_blocking_findings = true` sends each otherwise
uncorroborated blocker or major through a targeted Fable/high adversarial pass
when every required reviewer and check completed and the result can still
change. The cross-examiner must trace the concrete trigger-to-impact path and
may confirm, demote, or disprove the candidate. Confirmed findings remain open;
demoted findings retain their effective non-blocking severity; disproved
findings remain in the audit record but no longer block approval. An incomplete
cross-examination fails closed. The independent `[cross_examination]` timeout,
turn ceiling, and cost ceiling keep this targeted phase bounded separately from
ordinary and security-sensitive Claude reviews.

Findings whose complete provenance is backed by completed reviewers are carried
forward whenever the base, head, and exact diff hash are unchanged. Later
reviewers receive those findings as historical evidence, and simple omission
does not erase them; an explicit cross-examination rejection retires a carried
finding. Partial-only or mixed partial recovery evidence remains visible in its
original run but requires fresh confirmation before it can become authoritative.
Source run IDs are preserved in the normalized finding and manifest;
`decision.json` records the completed-reviewer-backed continuity set separately
as `carry_forward_findings`.

Broad disagreement adjudication remains opt-in because it adds the cost of a
complete third review. Pass `--adjudicate` or set
`adjudicate_disagreements = true` to retain that independent Fable/high report
in addition to the targeted blocking-finding policy.

Every initial blocker or major must also include demonstrated reachability: an
external trigger, an ordered code/data/control path through relevant guards and
transformations, the observable impact, and required preconditions. A serious
claim without that evidence is an incomplete reviewer result, not a blocking
finding. Non-blocking findings may use `reachability.status = "not_applicable"`
when trigger-to-impact analysis genuinely does not apply; the schema and runtime
validator accept the same status set.

`effort` accepts `low`, `medium`, `high`, `xhigh`, or `max`; Codex also accepts
`none` and `minimal`. Codex defaults to `gpt-5.6-sol` at high effort so its effective
selection and API-equivalent pricing are reproducible even when user CLI
defaults change.

The policy fails closed. A timeout, malformed response, missing reviewer,
incomplete context, or interrupted check produces `incomplete`, never an
approval. Reviewer processes run in parallel and are killed as process groups
at their deadlines. Corroborated or cross-exam-confirmed blocking findings and
explicit `request_changes` verdicts without a corresponding adjudicable finding
win; otherwise an abstention requires human adjudication.

For deterministic, subscription-first execution, the Codex adapter requires a
ChatGPT login, ignores user CLI configuration, and forces a network-disabled
workspace-write sandbox around its disposable clone.
On macOS, CORA also discovers the Codex CLI bundled with ChatGPT when `codex`
is not otherwise available on `PATH`.
The Claude adapter requires first-party Claude.ai subscription authentication
and runs with safe mode plus Claude's strict Bash sandbox: network access and
unsandboxed command fallback are denied, source-editing tools are unavailable,
and sandbox startup failure is terminal. When explicitly requested, only the
parent Cora process performs the bounded web-evidence capture described above;
reviewer processes still cannot access the network. Both reviewers may run focused local
tests; each gets a private temporary/cache directory for tools such as Go and
Vitest. Common API-key environment variables are removed unless
`--allow-api-billing` is explicitly passed.

Provider concurrency uses a user-global FIFO queue across Cora processes and
repositories. `cora status --active` reports each reviewer's position, number
ahead, and a best-effort ETA derived from recent executions. The first estimate
is stored as an absolute deadline, so subsequent heartbeats count down instead
of moving the ETA forward. Queue wait is recorded separately from execution
time and does not consume reviewer or overall execution timeouts;
`queue_timeout` bounds the wait itself. Claude defaults to one concurrent
request and Codex to two; adjust
`max_concurrency` per reviewer when the subscription permits it. Quota failures
are marked retryable and their reset time is saved when the CLI reports one. A
future reset is also shared through the global provider queue, so concurrent
waiters and later runs return a resumable quota result without invoking the
provider again before that time. Runs waiting for that reset remain visible in
`cora status --active` as quota-queued work.

`cora retry` creates a child run, walks its exact-diff parent lineage, reuses
the newest completed result for every unselected reviewer, and queues only the
selected provider. Targeted `claude-security` results remain distinct from the
ordinary Claude review and retain their audited model and effort when retried.
Completed Fable adjudication and cross-examination work is also retained when
its exact candidate set is unchanged, avoiding another expensive targeted pass.
It also recovers reset timestamps from older saved Claude errors that predate
the structured retry field, including hour-only messages such as `resets 4am`.
`--no-wait` returns immediately when a saved reset time is still in the future.
`--reviewer-timeout`, `--overall-timeout`, and `--max-turns` may only increase
the saved limits. They are applied after restoring the parent policy, mapped to
the selected ordinary or targeted reviewer roles, and recorded explicitly in
the child manifest along with the complete resulting effective policy. Cora
also persists each role's effective timeout and turn ceiling, so a targeted
security retry cannot broaden an ordinary review or later adjudication retry.

A web-backed retry is deliberately whole-review rather than selective. Cora
first validates and copies the frozen snapshot without refetching it, then
reuses no ordinary or conditional reviewer result. Every ordinary reviewer runs
again against those same bytes, as does any security, adjudication, or
cross-examination role required by the retry's outcome. Explicit `--reviewer`
selection is rejected for a web-backed run.

Claude's `finalization_turns` are enforced rather than advisory. Cora caps the
tool-enabled inspection process at `max_turns - finalization_turns`; if that
boundary is reached, a second process receives the reserved turns with tools
disabled and only the best persisted inspection evidence. The finalizer may
serialize that evidence into the required schema, but it cannot change the
verdict, context status, findings, reviewed paths, omitted paths, or residual
risks. When `max_budget_usd` is set, it receives only the known remaining
whole-review budget and is skipped fail-closed if that budget cannot be
established.

After every reviewer and at the end of a run, CORA prints the effective model,
effort, provider-reported turns, thinking tokens, API-equivalent cost, and that
reviewer's verdict as soon as it completes. An incomplete reviewer line also
includes the provider's normalized failure immediately. The normalized values
are saved per reviewer and checkpointed in `manifest.json` and `heartbeat.json`
as each reviewer finishes, then aggregated in `decision.json`.
Retry records distinguish incremental usage for that attempt from cumulative
usage across their parent lineage; the compatibility `usage` field is
cumulative. Claude cost comes from the CLI result envelope; Codex cost is
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

Equivalent findings are consolidated before cross-examination using location,
claim, evidence, suggested-fix, and reachability similarity, while each
original reviewer report remains intact. This prevents a severity disagreement
about the same defect from triggering a redundant Fable pass.
`cora show` includes consolidated confidence, evidence, suggested fixes, and
residual risks by default. `cora show --verbose` additionally expands each
original reviewer report, including omitted paths.

## Records

Run records are stored beneath the repository's Git common directory:

```text
.git/cora/runs/<run-id>/
.git/cora/web-evidence-runs-v1/<run-id>/
.git/cora/auto-fix/<loop-id>/
```

This keeps local records shared by Git worktrees without adding review output
to the source tree. The versioned web collection keeps evidence-dependent
records invisible to readers that predate the binding rules. Its manifests use
`approved-baseline-delta` as a raw, old-reader-visible compatibility sentinel;
current Cora validates the guarded record and reports its effective scope as
`full`. Each record includes the canonical patch, exact prompt and schema, raw
tool logs, normalized reviewer reports, check logs, any captured web-evidence
bodies/index/prompt, manifest, event stream, and deterministic decision. Each
auto-fix parent manifest links its child review runs and stores every
coding-agent prompt, pre/post patch, raw log, usage record, limit, and stop
reason. Publishing signed records to a dedicated Git ref is planned as a
separate command.

Each active review run and auto-fix parent updates `heartbeat.json` every 30
seconds. Auto-fix heartbeats include the current iteration, phase, elapsed time,
and cumulative usage; the invoking terminal prints the same lifecycle progress.
Ordinary reviews record wall elapsed time separately from approximate active
execution time. Active time is sampled only while work is running, excludes
provider queues, and discounts long sampling gaps caused by machine sleep;
records label this basis explicitly. Running-reviewer durations remain labeled
as wall time. `cora status --active`
shows concurrent runs in one table with reviewer elapsed time and fixed-deadline
queue ETA countdowns. Once a historical estimate elapses, Cora shows the active
capacity holder and its remaining execution timeout instead of a zero or a
sliding replacement estimate. SIGINT and SIGTERM cancel the complete reviewer
process group, remove disposable workspaces, and release owned run and provider
locks; abandoned run locks are reclaimed after their owner exits. `cora list`
supports state and head-SHA filters. `latest` is resolved by run start time
instead of completion order, so concurrent reviews cannot overwrite its
meaning.

Builds embed the Cora source SHA and UTC build time. Manifests record those
values plus a credential-free repository identity such as
`github.com/herikwebb/cora`; repositories without a remote fall back to their
root commit identity.

The current record is local and is not cryptographically signed. Treat Git-ref
publication, signatures, and a GitHub status-check bridge as follow-on work
before using CORA as an organization-wide enforcement boundary.
