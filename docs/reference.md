# Configuration and behavior

For the first review, start with the [README](../README.md). For complete
settings and defaults, see [examples/config.toml](../examples/config.toml).

- [Configuration sources](#configuration-sources)
- [Reviewers and authentication](#reviewers-and-authentication)
- [Consensus and findings](#consensus-and-findings)
- [Security review and adjudication](#security-review-and-adjudication)
- [Incomplete reviews and recovery](#incomplete-reviews-and-recovery)
- [Provider isolation](#provider-isolation)
- [Validation checks](#validation-checks)
- [External validation evidence](#external-validation-evidence)
- [Web evidence](#web-evidence)
- [Concurrency and quotas](#concurrency-and-quotas)
- [Retries](#retries)
- [Usage reporting](#usage-reporting)
- [Saved records](#saved-records)
- [Exit codes](#exit-codes)

## Configuration sources

CORA starts with embedded defaults, then loads personal settings from the
operating system's user configuration directory, then repository settings from
`.cora/config.toml`. Repository values override personal values.

```bash
cora config path
```

This prints the exact personal configuration path. Common paths are
`~/.config/cora/config.toml` on Linux and
`~/Library/Application Support/cora/config.toml` on macOS.

Repository configuration and relative reviewer prompts are read from the
resolved **base revision**. A reviewed change therefore cannot configure its
own reviewers or checks. Changes to `.cora/config.toml` on a feature branch do
not configure that branch's review; use personal settings or update the trusted
base first.

Use [`cora plan`](workflows.md#preview-a-review) to inspect the effective target,
policy, reviewers, and checks without running them.

## Reviewers and authentication

The current adapters invoke installed provider CLIs and reuse their logins;
they do not call provider SDKs directly. Codex and Claude are enabled by default.
Gemini is an optional third reviewer.

| Reviewer | Default model | Default effort | Default concurrency |
| --- | --- | --- | ---: |
| Codex | `gpt-5.6-sol` | `high` | 2 |
| Claude | `opus` | `high` | 1 |
| Gemini (optional) | `gemini-2.5-pro` | Not supported | 1 |

Codex requires a ChatGPT login by default. On macOS, CORA can find the Codex CLI
bundled with ChatGPT when `codex` is not on `PATH`. Claude requires first-party
Claude.ai subscription authentication by default.

For Codex and Claude, `effort` accepts `low`, `medium`, `high`, `xhigh`, or `max`.
Codex also accepts `none`, `minimal`, and `ultra`. Ultra runs keep their session
history so subagents can inherit the parent context; other efforts use ephemeral
sessions. Sandbox and configuration isolation remain the same. An explicit
default Codex model keeps selection and API-equivalent pricing reproducible when
CLI defaults change.

### Claude ultracode

Set `reviewers.claude.effort = "ultracode"` to enable Claude Code's built-in
workflow and subagent orchestration at `xhigh` effort. `escalation.effort` also
accepts `ultracode` for focused security reviews, adjudication, and blocking-finding
cross-examination. This requires
Claude Code **2.1.205 or later** so delegated reviewers receive CORA's review
policy. The default remains `high`.

CORA enables workflows for these runs, including on Pro plans, while Claude
Code's model and organization restrictions still apply. See Claude Code's
[effort levels](https://code.claude.com/docs/en/model-config#adjust-effort-level)
and [workflows](https://code.claude.com/docs/en/workflows) documentation.

### Enable Gemini

Gemini requires Gemini CLI **0.46.0 or later** and a cached Google login from
running `gemini`. Add this to your personal configuration or the trusted base
revision's `.cora/config.toml`:

```toml
minimum_approvals = 3

[reviewers.gemini]
enabled = true
```

This enables Gemini and raises the approval threshold to three. Keeping
`minimum_approvals = 2` leaves the threshold at two. Every enabled reviewer must
still complete with full context; enabling Gemini alone does not raise the
threshold. See [consensus and findings](#consensus-and-findings) for how
cross-examination can contribute to the approval count.

CORA supplies Gemini with isolated configuration and reuses cached OAuth
credentials. Gemini supports `max_turns`; `effort`, `finalization_turns`, and
`max_budget_usd` must remain unset or empty/zero. Vertex AI authentication is
not supported. See [Gemini isolation](#gemini-isolation) for source restrictions
and [auto-fix](auto-fix.md#limits-and-stopping-conditions) for its current usage
metric limitation.

### API-key billing

API-key authentication is refused by default. Use `--allow-api-billing` or the
trusted `allow_api_billing` setting only when separately billed usage is
intentional. Common API-key environment variables are otherwise removed.
For Gemini, explicit authorization permits `GEMINI_API_KEY` or
`GOOGLE_API_KEY`. This still uses the provider CLI adapter.

## Consensus and findings

`minimum_approvals = 2` sets the default approval threshold. It can be raised up
to the number of enabled reviewers. Every enabled reviewer must complete with
full context, regardless of the threshold. Successful cross-examination can
contribute one approval after resolving disputed blocking findings, so the
threshold does not require every ordinary reviewer to return a literal
`approve` verdict. Auto-fix has a stricter
[all-reviewers-approve requirement](auto-fix.md#how-the-loop-works).

By default, `blocker` and `major` findings block approval. Strict mode
(`--strict` or `strict = true`) also blocks on `minor` findings and requires at
least one validation check. Notes remain non-blocking.

A corroborated or cross-examination-confirmed blocking finding, a failed check,
or an explicit `request_changes` verdict without an adjudicable finding
requests changes. Otherwise, an abstention requires a human decision. A timeout,
malformed response, missing reviewer, incomplete context, or interrupted check
produces `incomplete`, never approval.

An approval with minor or note findings displays as
`APPROVED WITH NON-BLOCKING FINDINGS`. Its machine state remains `approved`
and its exit code remains `0`.

### Evidence and continuity

Every initial blocker or major must demonstrate reachability: an external
trigger, the ordered code/data/control path through relevant guards and
transformations, observable impact, and required preconditions. A serious claim
without this evidence makes the reviewer result incomplete. Non-blocking
findings may use `reachability.status = "not_applicable"` when this analysis does
not apply.

Equivalent findings are consolidated before cross-examination using location,
claim, evidence, suggested fix, and reachability similarity. Original reports
remain intact. A severity disagreement about the same defect therefore does
not trigger a redundant Fable pass.

Findings fully supported by completed reviewers carry forward while the base,
head, and exact diff hash remain unchanged. Later reviewers receive them as
historical evidence; omission does not erase them. Explicit rejection by
cross-examination retires a carried finding. Partial-only or mixed partial
recovery evidence needs fresh confirmation before becoming authoritative.
Source run IDs remain in findings and manifests; `decision.json` records the
continuity set as `carry_forward_findings`.

## Security review and adjudication

Changes to reviewer/control files or paths matching
`escalation.security_path_markers` add a focused Fable/high security review.
`--security-sensitive` forces the same pass when paths do not capture the risk.
It supplements the ordinary full-diff Opus/high review.

The pass covers sensitive paths and the callers, callees, trust boundaries,
configuration, and deployment flow needed to establish concrete reachability.
Its result, effective model/effort, usage, prompt hash, and prompt are recorded
under `security_reviews` and `security-review.prompt.md`.

This required pass cannot approve with incomplete context, abstention, or a
blocking finding. Its scoped approval does not count toward the ordinary
quorum. A finding from this focused Fable pass blocks directly without a
redundant Fable cross-examination.

`escalation.max_turns` and `escalation.max_budget_usd` override ordinary Claude
ceilings for security and broad adjudication passes. Omit either to inherit the
Claude value; explicitly set `max_budget_usd = 0` to remove an inherited cost
ceiling. CORA defers a Fable pass if an independent completed result has already
fixed the outcome and the pass cannot change it. A potentially disprovable,
uncorroborated major still receives targeted reachability adjudication.

### Cross-examine blocking findings

With `cross_examine_blocking_findings = true` (the default), an otherwise
uncorroborated blocker or major receives a targeted Fable/high adversarial pass
when every required reviewer and check completed and the result can still
change. The cross-examiner traces the trigger-to-impact path and can:

- Confirm the finding, keeping it open.
- Demote it to a non-blocking severity.
- Disprove it, retaining the audit entry without blocking approval.

Incomplete cross-examination cannot approve. The `[cross_examination]` timeout,
turn ceiling, and cost ceiling bound this work separately from ordinary and
security reviews.

Broad disagreement adjudication is separate and opt-in because it adds a full
review. Pass `--adjudicate` or set `escalation.adjudicate_disagreements = true`
to retain that additional Fable/high report.

## Incomplete reviews and recovery

Codex and Claude checkpoint confirmed findings only when the evidence changes.
On a timeout or Claude turn ceiling, CORA retains valid provider output or a
checkpoint as a partial abstaining report with incomplete context. This
evidence remains visible but cannot approve the run. A finding supported only
by partial evidence is not carried forward as authoritative.

Recovery files live in a randomized private directory, separate from the
temporary/cache directory inherited by repository tests. `max_budget_usd`
provides an additional Claude Code cost ceiling.

### Claude finalization

CORA caps tool-enabled inspection at `max_turns - finalization_turns`. If that
boundary is reached, a second, tools-disabled process receives the reserved
turns and the best persisted inspection evidence.

For ultracode reviews, `max_turns` limits the parent conversation; it does not
cap the combined turns of its subagents. The reviewer timeout and optional
`max_budget_usd` cost ceiling still apply. The finalizer uses `xhigh` effort
with workflows and all tools disabled.

The finalizer can serialize the evidence into the required schema. It cannot
change the verdict, context status, findings, reviewed or omitted paths, or
residual risks. With a cost ceiling, it receives only the known remaining
whole-review budget. If that remainder cannot be established, finalization is
skipped and the review remains incomplete.

## Provider isolation

Each reviewer and local-check phase receives an independent local clone at the
exact target. CORA removes all remotes before execution and discards the clone
afterward. Generated files and disposable Git changes do not affect the user's
checkout or repository refs. The [auto-fix coding agent](auto-fix.md) is a
separate, explicitly write-enabled step in the user's working tree.

Codex ignores user CLI configuration and runs in a network-disabled
`workspace-write` sandbox around its disposable clone. Claude uses safe mode
and a strict Bash sandbox: networking, unsandboxed fallback, and source-editing
tools are unavailable. Sandbox startup failure ends the review.

Claude normally receives `Read`, `Glob`, `Grep`, and `Bash`. Ultracode also
receives `Agent`, `Workflow`, and `TaskStop` for orchestration. The same sandbox
and network restrictions apply. Conversation transcript persistence remains
disabled; Claude Code can still store workflow scripts and execution records.

Codex and Claude may run focused local tests. Each gets private temporary/cache
directories for tools such as Go and Vitest. Only the parent CORA process may
capture explicitly authorized web evidence; reviewer processes remain offline.

### Gemini isolation

Gemini receives the exact diff as a file and inspects a disposable source mirror
with read, list, and grep tools. The mirror omits Git metadata and keeps
`GEMINI.md` contents under inert filenames, with a mapping back to original
paths. Reports use the original repository paths.

A snapshot containing a symlink makes Gemini incomplete; CORA neither omits it
silently nor follows it outside the snapshot. Machine-wide Gemini policy files
also make the review incomplete because they override CORA's isolated policy.

The isolated configuration disables shell commands, tests, source edits,
network tools, MCP, extensions, hooks, and repository instruction loading.
These are CLI tool restrictions, not an operating-system sandbox; they do not
provide the same containment as the Codex and Claude sandboxes.

## Validation checks

Configured checks execute code from the reviewed tree. CORA requires
`--allow-unsafe-checks` or trusted `allow_unsafe_host_checks = true` before
running them.

Allowed checks run in a disposable, remote-free clone with a minimal environment
and ephemeral home and temporary directories. A check's `env_allowlist` adds
only named variables from the parent environment. These controls protect the
checkout and reduce credential exposure; they are **not a filesystem or network
sandbox**.

Named validation profiles group checks. Built-in profiles are:

| Profile | Auto-detection marker |
| --- | --- |
| `go` | `go.mod` |
| `node` | `package.json` |
| `python` | `pyproject.toml`, `pytest.ini`, or `setup.cfg` |

Use `--profile auto` to detect profiles, or select a name explicitly, such as
`--profile go`. Trusted configuration can add `[[validation_profiles]]`.
Profiles have the same host-check authorization requirement. When host checks
are enabled and neither a profile nor configured checks are present, CORA
auto-detects built-in profiles.

Without a configured validation check or passing imported attestation, CORA
records `validation_status = "not_run"` and a residual risk. Reviewer-selected
tests do not substitute for deterministic validation. Strict mode is incomplete
when no validation profile, configured check, or passing imported evidence is
available.

## External validation evidence

An external CI or test operator can supply a passing attestation using
repeatable `--validation-evidence PATH` flags. First use `cora plan --json` to
obtain `repository_identity`, `target.base_sha`, `target.head_sha`, and
`target.diff_hash`. Create a JSON file for that exact target:

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

CORA requires `status = "passed"` and rejects unknown or duplicate fields and
any repository/base/head/diff mismatch. Imported evidence counts as a validation
check. CORA copies the exact source bytes and SHA-256 hash into the private run
record and validates them again before retry, verification, or approval reuse.

`verifier` and `source` are operator-supplied labels, not cryptographic proof.
Importing the file is an explicit trust decision. CORA records `command` for
audit purposes and never executes it. Because evidence binds one exact diff,
`--validation-evidence` cannot be combined with `--auto-fix`.

## Web evidence

Web evidence is off by default. Pass `--allow-review-web` or set trusted
`allow_review_web = true`, then provide one or more `--web-evidence URL` values.
CORA fetches only the sources selected by the operator; it never discovers or
follows URLs from the reviewed repository.

Accepted URLs must use HTTPS, public DNS addresses, and the default HTTPS port.
Credentials, query strings, and fragments are rejected. Redirects must remain
on the explicitly selected origin. CORA ignores environment proxies, blocks
local, private, link-local, metadata, documentation, and other non-public IP
ranges at connection time, and sends an unauthenticated GET without cookies.

Capture limits are four sources, 32 KiB of UTF-8 text per source, and 64 KiB
across a review. Larger individual responses are marked truncated; capture
fails if the aggregate presentation exceeds its limit.

The private `web-evidence/` record stores the bounded bodies, strict metadata
index, and exact prompt appendix. `manifest.json` includes individual SHA-256
hashes and a composite `snapshot_sha256` binding the index and appendix. The
appendix JSON-escapes and labels pages as untrusted corroborating material.
Ordinary, security, adjudication, and cross-examination reviewers all receive
the same bytes.

Retries validate and copy the exact snapshot without refetching it, then rerun
all ordinary and required conditional roles. `--reviewer` selection is rejected
for web-backed retries. Refreshing evidence requires a fresh review.

Web evidence does not count as a validation check, cannot accompany
`--auto-fix`, and cannot seed an auto-fix approval baseline. It does not enable
provider-native WebFetch, WebSearch, MCP, or shell networking. Codex, Claude,
Fable, reviewer-selected tests, and Bash retain their network restrictions;
Gemini has no network tools.

Web-backed records use a versioned collection that older CORA binaries do not
enumerate. A compatibility sentinel also prevents older delta-aware readers
from retrying, verifying, or reusing these approvals. Upgrade CORA to inspect
or retry them. Current readers validate the guard and report effective review
scope `full`; see [saved records](#saved-records).

## Concurrency and quotas

Reviewer processes run in parallel and are killed as process groups at their
deadlines. A user-global FIFO queue shares provider capacity across CORA
processes and repositories. Configure `max_concurrency` per reviewer for the
available subscription capacity.

For Claude ultracode, this caps CORA's Claude processes; it does not limit the
subagents started within each workflow.

`cora status --active` shows queue positions, counts ahead, and best-effort ETAs
from recent executions. The initial estimate is stored as an absolute deadline,
so heartbeats count down instead of moving the estimate forward. Once an
estimate elapses, status shows the active capacity holder and remaining
execution timeout.

Queue wait is recorded separately and does not consume reviewer or overall
execution timeouts. `queue_timeout` bounds the wait itself.

Quota failures are retryable; CORA saves the reset time when the CLI reports
one. A future reset is shared through the global queue, preventing concurrent
waiters and later runs from invoking the provider before that time. Work
waiting for the reset remains visible in `cora status --active`.

## Retries

`cora retry` creates a child run and walks the exact-diff parent history. It
reuses the newest completed result for each unselected reviewer and queues
only selected providers.

Targeted `claude-security` results stay separate from ordinary Claude results
and retain their audited model and effort. Completed Fable adjudication and
cross-examination are retained when the exact candidate set is unchanged.
CORA can also recover reset times from older saved Claude errors, including
hour-only messages such as `resets 4am`. `--no-wait` returns immediately when
a saved reset remains in the future.

`--reviewer-timeout`, `--overall-timeout`, and `--max-turns` may only increase
saved limits. CORA restores the parent policy, applies overrides to the selected
ordinary or targeted roles, and records them alongside the full effective
policy in the child manifest. Each role's timeout and turn ceiling are saved
separately, so a security retry cannot broaden ordinary or later adjudication
limits.

Web-backed retries are the exception to selective reuse: they validate and copy
the frozen snapshot, reuse no reviewer result, and run every ordinary and
required conditional role against the same bytes. `--reviewer` is rejected.

## Usage reporting

After each reviewer finishes and at the end of the run, CORA reports effective
model, effort, provider-reported turns, thinking tokens, API-equivalent cost,
and verdict. An incomplete reviewer also displays its normalized provider
failure immediately.

Values are checkpointed in `manifest.json` and `heartbeat.json` as reviewers
finish, then aggregated in `decision.json`. Retries distinguish usage added by
the attempt from cumulative usage across the parent history; the compatibility
`usage` field is cumulative.

Claude cost comes from its CLI result envelope. Codex cost is calculated from
reported tokens and the pricing table named by `cost_source`. Unavailable
metrics display as `n/a`; mixed totals display as `partial`. Gemini reports
tokens, but authoritative turn count and API-equivalent cost are unavailable
and are not estimated.

Provider failures also appear in the top-level decision, so JSON callers do not
need raw logs to diagnose them. JSON durations use integer `duration_ms` and
`elapsed_ms` fields, not Go's nanosecond representation.

## Saved records

Records live under the repository's Git common directory, shared by worktrees:

```text
.git/cora/runs/<run-id>/
.git/cora/web-evidence-runs-v1/<run-id>/
.git/cora/auto-fix/<loop-id>/
```

Records contain the canonical patch, exact prompt and schema, raw tool logs,
normalized reviewer reports, check logs, any web-evidence bodies/index/prompt,
manifest, event stream, and deterministic decision. Each review keeps its own
record rather than replacing previous feedback. Auto-fix parents additionally
link child reviews and record coding-agent inputs, patches, logs, usage, limits,
and stop reasons.

The versioned web collection keeps evidence-dependent records invisible to
older readers. Its manifests use `approved-baseline-delta` as the raw
compatibility sentinel; current CORA validates the guarded record and reports
the effective scope as `full`.

Active reviews and auto-fix parents update `heartbeat.json` every 30 seconds.
Auto-fix heartbeats include iteration, phase, elapsed time, and cumulative
usage. Ordinary reviews distinguish wall elapsed time from approximate active
execution time. Active time excludes provider queues and discounts long
sampling gaps caused by machine sleep; records label this basis. Running
reviewer durations are labeled wall time.

SIGINT and SIGTERM cancel complete reviewer process groups, remove disposable
workspaces, and release owned run and provider locks. Abandoned run locks are
reclaimed after their owner exits. `cora list` supports state and head-SHA
filters. `latest` is determined by start time, not completion order.

Builds embed CORA's source SHA and UTC build time. Manifests record those values
and a credential-free repository identity, such as `github.com/herikwebb/cora`.
Repositories without a remote use their root commit identity.

Records are local and not cryptographically signed. Publishing records to a
dedicated Git ref, signatures, and a GitHub status-check bridge remain planned
work; these records are not an organization-wide enforcement boundary.

## Exit codes

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

[Back to the README](../README.md)
