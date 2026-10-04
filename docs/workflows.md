# Review workflows

Use these recipes after [installing CORA](../README.md). Replace
`upstream/main` with the base branch for your repository.

- [Choose a review target](#choose-a-review-target)
- [Preview a review](#preview-a-review)
- [Review before opening a pull request](#review-before-opening-a-pull-request)
- [Add checks or evidence](#add-checks-or-evidence)
- [Request an additional security review](#request-an-additional-security-review)
- [Retry an incomplete review](#retry-an-incomplete-review)
- [Inspect saved runs](#inspect-saved-runs)
- [Write and share findings](#write-and-share-findings)

## Choose a review target

```bash
# Review the current branch against a base branch.
cora review --base upstream/main

# Review one commit.
cora review --commit abc123

# Review an explicit commit range.
cora review --range abc123..def456

# Review staged, unstaged, and untracked changes.
cora review --uncommitted
```

Working-tree reviews are advisory: `--uncommitted` cannot create a final
approval attestation. For a pull request, check out its branch and review it
against the intended base. CORA does not accept a PR URL or number as a target.

## Preview a review

`cora plan` resolves the exact target and effective policy without creating a
run, acquiring a provider slot, executing a check, or invoking a reviewer.

```bash
cora plan --base upstream/main
cora plan --base upstream/main --strict --profile auto --allow-unsafe-checks
cora plan --base upstream/main --json
```

Planning reads repository configuration from the trusted base revision and
reports:

- The exact target and diff hash.
- Ordinary and conditional reviewer models, effort, and limits.
- Security triggers and matching paths.
- Expanded validation checks.
- Configured provider capacity and planned concurrency demand.

Live capacity remains unknown until a review acquires slots. Planned demand is
not an availability guarantee.

`plan` accepts the review targeting and policy flags: `--base`, `--commit`,
`--range`, `--uncommitted`, `--parent`, `--profile`, `--strict`,
`--security-sensitive`, `--adjudicate`, and billing, check, and web authorization
flags. It also accepts repeatable `--validation-evidence` paths and
`--web-evidence` HTTPS URLs. It validates these inputs without importing files
or fetching pages.

## Review before opening a pull request

The review loop runs locally and does not require an existing pull request:

```bash
# Review the committed branch changes.
cora review --base upstream/main

# If the exit code is 2, inspect findings, fix them, and commit your changes.
cora show latest

# Review again after committing the fixes.
cora review --base upstream/main

# Verify that approval still matches HEAD before opening the PR.
cora verify --head HEAD
```

After verification succeeds, create the PR through GitHub or its CLI:

```bash
gh pr create --repo OWNER/UPSTREAM --base main --head YOUR_FORK:YOUR_BRANCH
```

Every review creates a new run directory and preserves earlier feedback.
Coding agents can use `--json` and the documented [exit codes](reference.md#exit-codes)
as their control interface. For a bounded coding-agent loop, see
[auto-fix](auto-fix.md).

## Add checks or evidence

Strict mode makes minor findings block approval and requires validation.
The following command permits locally executed checks from the reviewed tree:

```bash
cora review --base upstream/main --strict --profile auto --allow-unsafe-checks
```

Host checks run in a disposable clone but are not a filesystem or network
sandbox. Use them only when you trust the reviewed code. See
[validation checks](reference.md#validation-checks) for profiles and environment
controls.

To import a passing test or CI result without running its command:

```bash
cora review --base upstream/main --validation-evidence /path/to/ci-evidence.json
```

The evidence must match the exact repository, base, head, and diff. See the
[attestation format](reference.md#external-validation-evidence).

To give every reviewer the same bounded snapshot of selected documentation:

```bash
cora review --base upstream/main --allow-review-web \
  --web-evidence https://pkg.go.dev/net/http \
  --web-evidence https://go.dev/security/vuln/
```

Only CORA fetches those pages; reviewers remain offline. Web evidence is
corroborating material, not a validation check. See
[web evidence](reference.md#web-evidence) for capture limits and retry behavior.

## Request an additional security review

Force a focused Fable/high security pass when path matching does not capture
the risk. The ordinary Opus/high review still runs:

```bash
cora review --base upstream/main --security-sensitive
```

Opt into an additional full Fable review to adjudicate disagreement:

```bash
cora review --base upstream/main --adjudicate
```

Broad adjudication can add substantial usage. It is separate from the default,
targeted cross-examination of uncorroborated blocking findings. See
[security review and adjudication](reference.md#security-review-and-adjudication).

## Retry an incomplete review

Retry only the reviewer that failed while retaining completed results:

```bash
cora retry latest --reviewer claude
cora retry latest --reviewer gemini --max-turns 65
```

Increase the limits that ended a previous attempt when needed:

```bash
cora retry latest --reviewer claude \
  --reviewer-timeout 30m --overall-timeout 1h --max-turns 65
```

Retries preserve the parent policy, model, effort, and evidence. Limit overrides
may only increase saved limits. A saved quota reset is respected; add
`--no-wait` to return immediately while the reset is still in the future.

Web-backed runs require a whole-review retry against the same frozen evidence;
omit `--reviewer` for those runs. See [retry rules](reference.md#retries).

## Inspect saved runs

```bash
cora status --active
cora list --state incomplete
cora show latest
cora show latest --verbose
cora show latest --json
cora verify --head HEAD
```

`show` includes consolidated confidence, evidence, suggested fixes, and
residual risks. `--verbose` adds each original reviewer report, including
omitted paths. `latest` means the most recently started run, even when concurrent
reviews finish in a different order.

## Write and share findings

CORA applies the same [review-writing rules](../prompts/review-writing.md) to
every reviewer, including repositories with custom prompts. Use plain terms,
short titles, and an explanation of the trigger, resulting problem, and needed
change. Avoid AI jargon and em dashes in new prose; preserve literal code and
quoted evidence.

Human-readable findings use explicit severity labels:

| Label | Stored severity |
| --- | --- |
| Severity: High | `blocker` or `major` |
| Severity: Medium | `minor` |
| Severity: Low | `note` |

Severity describes impact, separately from model effort, confidence, and the
review verdict. The display labels do not change the schema or approval policy.
Finding titles do not include P1/P2 or repeat the severity label.

Use the same style when copying a finding into a GitHub review:

> **Severity: High**
>
> **Retry can overwrite another workflow**
>
> If the original workflow was deleted and another workflow uses the same name,
> retry can replace the new workflow's contents. Check that the workflow belongs
> to this run before updating it. Add a test showing that an unrelated workflow
> with the same name stays unchanged.

Keep the code location and supporting evidence with the comment. Posting is a
separate step through GitHub or its CLI; CORA does not post reviews itself.

[Back to the README](../README.md)
