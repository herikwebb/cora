package verdict

import (
	"fmt"
	"time"

	"github.com/herikwebb/cora/internal/model"
)

func Evaluate(runID string, target model.Target, reviewers []model.ReviewerResult, checks []model.CheckResult, blocking []string, minimumApprovals int, now time.Time) model.Decision {
	decision := model.Decision{
		SchemaVersion: model.SchemaVersion,
		RunID:         runID,
		BaseSHA:       target.BaseSHA,
		HeadSHA:       target.HeadSHA,
		DiffHash:      target.DiffHash,
		Reviewers:     make(map[string]string, len(reviewers)),
		OpenFindings: map[string]int{
			"blocker": 0,
			"major":   0,
			"minor":   0,
			"note":    0,
		},
		Checks:    make(map[string]string, len(checks)),
		CreatedAt: now.UTC(),
	}

	blockingSet := make(map[string]bool, len(blocking))
	for _, severity := range blocking {
		blockingSet[severity] = true
	}

	if len(reviewers) == 0 {
		decision.State = model.StateIncomplete
		decision.Reason = "no reviewers were enabled"
		return decision
	}

	incomplete := false
	abstained := false
	changesRequested := false
	approvals := 0
	for _, reviewer := range reviewers {
		if reviewer.Status != "completed" || reviewer.Report == nil {
			decision.Reviewers[reviewer.Reviewer] = "incomplete"
			incomplete = true
			continue
		}
		report := reviewer.Report
		decision.Reviewers[reviewer.Reviewer] = report.Verdict
		if !report.ContextComplete {
			incomplete = true
		}
		switch report.Verdict {
		case "approve":
			approvals++
		case "request_changes":
			changesRequested = true
		case "abstain":
			abstained = true
		}
		for _, finding := range report.Findings {
			decision.OpenFindings[finding.Severity]++
			if blockingSet[finding.Severity] {
				changesRequested = true
			}
		}
	}

	checkIncomplete := false
	checkFailed := false
	for _, check := range checks {
		decision.Checks[check.Name] = check.Status
		switch check.Status {
		case "passed":
		case "failed":
			checkFailed = true
		default:
			checkIncomplete = true
		}
	}

	switch {
	case incomplete || checkIncomplete:
		decision.State = model.StateIncomplete
		decision.Reason = "one or more required reviews or checks did not complete"
	case changesRequested || checkFailed:
		decision.State = model.StateChangesRequested
		decision.Reason = "review findings or failed checks require changes"
	case abstained:
		decision.State = model.StateNeedsHuman
		decision.Reason = "a reviewer abstained and human adjudication is required"
	case approvals < minimumApprovals:
		decision.State = model.StateIncomplete
		decision.Reason = fmt.Sprintf("approval quorum not met: got %d, need %d", approvals, minimumApprovals)
	case !target.Finalizable:
		decision.State = model.StateIncomplete
		decision.Reason = "working-tree reviews are advisory and cannot create an approval"
	default:
		decision.State = model.StateApproved
		decision.Reason = fmt.Sprintf("all %d reviewers approved the exact change", len(reviewers))
	}
	return decision
}
