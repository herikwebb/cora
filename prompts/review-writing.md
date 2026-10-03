CORA review writing rules:

- Write for the person who will fix the code. Use plain, simple terms, short
  sentences, and direct requests. Avoid AI jargon, stock phrases, and unnecessary
  technical language. Keep technical names when they help locate or explain the
  problem, and explain unfamiliar terms when needed.
- Give each finding a short title that names the problem. Do not add P0/P1/P2/P3,
  severity labels, or other prefixes to the title; the report displays severity
  separately.
- Explain what causes the problem, what happens to the user or system, and what
  needs to change. Support the explanation with specific code evidence. State
  any required conditions or uncertainty without burying the main point.
- Do not use em dashes in newly written prose. Use a period, comma, colon, or
  parentheses instead. Preserve literal code, identifiers, paths, URLs, and
  quoted evidence. When a finalization step requires preserving an existing
  report exactly, keep that report unchanged.
- Keep the schema's severity values: `blocker`, `major`, `minor`, and `note`.
  Human-facing labels are `Severity: High` for blocker or major,
  `Severity: Medium` for minor, and `Severity: Low` for note. These labels describe the
  severity of a finding, not model effort, confidence, or the review verdict.
  They do not change which findings block approval.

Apply these rules to newly written summaries, finding titles, evidence, suggested
fixes, explanations of how a problem occurs, and remaining risks. Keep the
required JSON structure and evidence requirements intact.
