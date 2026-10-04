# Auto-fix

Auto-fix adds a bounded Codex coding-agent loop after review. It is opt-in and
requires a clean, checked-out feature branch.

```bash
cora review --base upstream/main --auto-fix --until minor --max-iterations 5
```

Replace `upstream/main` with your base branch. Review the resulting edits before
committing them: CORA never commits or reverts the coding agent's changes.

## How the loop works

After each review, CORA sends consolidated findings at or above `--until` to a
separately configured Codex coding agent in `workspace-write` mode. The agent
may edit the current working tree. CORA instructs it not to commit, change
branches or Git refs, use the network, push, or open a pull request, and checks
that `HEAD` has not moved before continuing.

If the initial exact diff already has a compatible approval, CORA preserves it
as a baseline and reviews the cumulative coding-agent delta. Compatibility
includes strictness, reviewer quorum and settings, required security review,
and the exact validation checks. A weaker or differently configured approval
cannot be reused. Web-backed approvals cannot be reused because auto-fix does
not replay web evidence into its review history.

Every review uses an exact snapshot. After the delta is approved, CORA always
runs a fresh full review against the original merge base. This final review
includes committed branch changes, agent edits, and untracked files. Validation
checks run in disposable clones containing that snapshot.

Auto-fix approval requires all of the following:

- The ordinary CORA approval policy passes.
- Every configured reviewer returns `approve`.
- Every required check passes.
- No open finding remains at or above the selected threshold.

An adjudicated disagreement is therefore insufficient for auto-fix approval.
An approved delta alone is also insufficient: the final full review must pass.

## Limits and stopping conditions

`--until` accepts `blocker`, `major`, or `minor`. It controls which findings the
agent attempts to fix; it never weakens the normal blocking policy.

The trusted base revision's `[auto_fix]` settings define the agent and loop
limits. See the [complete configuration example](../examples/config.toml).
These flags override the defaults when starting a loop:

| Flag | Controls |
| --- | --- |
| `--until` | Lowest severity the agent attempts to fix |
| `--max-iterations` | Maximum review iterations |
| `--max-duration` | Maximum loop duration |
| `--max-turns` | Cumulative review and coding-agent turns |
| `--max-cost-usd` | Cumulative API-equivalent cost |
| `--agent-timeout` | Time allowed for each coding-agent step |

The loop stops without approving on incomplete reviews, abstentions, failed
checks, agent failures, repeated equivalent findings, unchanged patches,
Git-state changes, or a configured limit.

Review and coding-agent turns and API-equivalent cost accumulate across the
whole loop. Provider CLIs report final usage only after a process exits, so a
single step can cross a turn or cost ceiling. CORA records that step and refuses
to continue or approve. If a provider does not expose the usage needed to
enforce a ceiling, the loop stops incomplete.

**Gemini currently prevents auto-fix completion.** Gemini reports tokens but
does not expose an authoritative turn count or API-equivalent cost. Auto-fix
requires both metrics, so a loop with Gemini enabled stops incomplete after
review. Use Codex and Claude for auto-fix until these metrics are available.

External validation evidence and web evidence are bound to an exact diff and
cannot be combined with `--auto-fix`. A web-backed approval cannot seed a
baseline either.

## Resume after a quota reset

A provider quota failure with a known retry time pauses the parent loop and
returns exit code `6`. Completed ordinary reviews, security reviews,
adjudication, cross-examination, and checks are retained for their exact diff.

After the reported reset, resume the same loop:

```bash
cora status --active
cora review --auto-fix --resume <loop-id>
```

Resume restores the recorded policy, checks, security classification, reviewer
settings, and loop limits. Configuration changes cannot silently weaken the
paused loop. Do not pass a new base, policy, or limit override with `--resume`.
Paused loops remain visible in `cora status --active`.

## Inspect the result

Successful and partial edits remain in the feature branch's working tree for
inspection, correction, and an explicit user-created commit. CORA neither
commits nor reverts them.

Each loop has a parent record under `.git/cora/auto-fix/<loop-id>/`, with links
to child review runs. It retains every coding-agent prompt, pre/post patch, raw
log, usage record, limit, and stop reason. Heartbeats report the iteration,
phase, elapsed time, and cumulative usage; the terminal reports the same
lifecycle progress.

See [exit codes](reference.md#exit-codes) for automation and
[saved records](reference.md#saved-records) for record details.

[Back to the README](../README.md)
