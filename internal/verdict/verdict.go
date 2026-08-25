package verdict

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

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
	var findings []findingWithReviewer
	var residualRisks []string
	verdictNames := make(map[string][]string)
	for _, reviewer := range reviewers {
		if reviewer.Status != "completed" || reviewer.Report == nil {
			decision.Reviewers[reviewer.Reviewer] = "incomplete"
			incomplete = true
			continue
		}
		report := reviewer.Report
		decision.Reviewers[reviewer.Reviewer] = report.Verdict
		verdictNames[report.Verdict] = append(verdictNames[report.Verdict], reviewer.Reviewer)
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
			findings = append(findings, findingWithReviewer{reviewer: reviewer.Reviewer, finding: finding})
			if blockingSet[finding.Severity] {
				changesRequested = true
			}
		}
		residualRisks = append(residualRisks, report.ResidualRisks...)
	}
	decision.Findings = consolidateFindings(findings)
	for _, finding := range decision.Findings {
		decision.OpenFindings[finding.Severity]++
	}
	decision.ResidualRisks = uniqueStrings(residualRisks)
	if len(verdictNames) > 1 {
		parts := make([]string, 0, len(verdictNames))
		for verdict, names := range verdictNames {
			sort.Strings(names)
			parts = append(parts, verdict+"="+strings.Join(names, ","))
		}
		sort.Strings(parts)
		decision.Disagreements = append(decision.Disagreements, "reviewer verdicts differ: "+strings.Join(parts, "; "))
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

type findingWithReviewer struct {
	reviewer string
	finding  model.Finding
}

func consolidateFindings(items []findingWithReviewer) []model.ConsolidatedFinding {
	var consolidated []model.ConsolidatedFinding
	for _, item := range items {
		match := -1
		for index := range consolidated {
			if equivalentFinding(consolidated[index], item.finding) {
				match = index
				break
			}
		}
		if match < 0 {
			consolidated = append(consolidated, model.ConsolidatedFinding{
				ID: stableFindingID(item.finding), Severity: item.finding.Severity,
				Confidence: item.finding.Confidence, File: item.finding.File, Line: item.finding.Line,
				Claim: item.finding.Claim, Evidence: nonEmptyStrings(item.finding.Evidence),
				SuggestedFixes: nonEmptyStrings(item.finding.SuggestedFix), Reviewers: []string{item.reviewer},
				SourceIDs: nonEmptyStrings(item.finding.ID),
			})
			continue
		}
		merged := &consolidated[match]
		if severityRank(item.finding.Severity) > severityRank(merged.Severity) {
			merged.Severity = item.finding.Severity
		}
		if item.finding.Confidence > merged.Confidence {
			merged.Confidence = item.finding.Confidence
			merged.Claim = item.finding.Claim
		}
		merged.Evidence = appendUniqueString(merged.Evidence, item.finding.Evidence)
		merged.SuggestedFixes = appendUniqueString(merged.SuggestedFixes, item.finding.SuggestedFix)
		merged.Reviewers = appendUniqueString(merged.Reviewers, item.reviewer)
		merged.SourceIDs = appendUniqueString(merged.SourceIDs, item.finding.ID)
	}
	for index := range consolidated {
		sort.Strings(consolidated[index].Reviewers)
		sort.Strings(consolidated[index].SourceIDs)
	}
	sort.Slice(consolidated, func(i, j int) bool {
		if severityRank(consolidated[i].Severity) != severityRank(consolidated[j].Severity) {
			return severityRank(consolidated[i].Severity) > severityRank(consolidated[j].Severity)
		}
		if consolidated[i].File != consolidated[j].File {
			return consolidated[i].File < consolidated[j].File
		}
		return consolidated[i].Line < consolidated[j].Line
	})
	return consolidated
}

func equivalentFinding(existing model.ConsolidatedFinding, candidate model.Finding) bool {
	if strings.EqualFold(strings.TrimSpace(existing.File), strings.TrimSpace(candidate.File)) == false {
		return false
	}
	if existing.Line > 0 && candidate.Line > 0 && abs(existing.Line-candidate.Line) > 3 {
		return false
	}
	left := findingTokens(existing.Claim)
	right := findingTokens(candidate.Claim)
	if jaccard(left, right) >= 0.45 {
		return true
	}
	for _, evidence := range existing.Evidence {
		if normalizedText(evidence) != "" && normalizedText(evidence) == normalizedText(candidate.Evidence) {
			return true
		}
	}
	return false
}

func findingTokens(value string) map[string]bool {
	stop := map[string]bool{"a": true, "an": true, "and": true, "the": true, "to": true, "of": true, "in": true, "is": true, "can": true, "could": true, "may": true}
	words := strings.FieldsFunc(strings.ToLower(value), func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsDigit(character)
	})
	tokens := make(map[string]bool)
	for _, word := range words {
		if len(word) > 1 && !stop[word] {
			tokens[word] = true
		}
	}
	return tokens
}

func jaccard(left, right map[string]bool) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	intersection := 0
	union := make(map[string]bool, len(left)+len(right))
	for token := range left {
		union[token] = true
		if right[token] {
			intersection++
		}
	}
	for token := range right {
		union[token] = true
	}
	return float64(intersection) / float64(len(union))
}

func stableFindingID(finding model.Finding) string {
	value := strings.ToLower(strings.TrimSpace(finding.File)) + fmt.Sprintf(":%d:", finding.Line) + normalizedText(finding.Claim)
	hash := sha256.Sum256([]byte(value))
	return "cora-" + hex.EncodeToString(hash[:6])
}

func normalizedText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func severityRank(severity string) int {
	switch severity {
	case "blocker":
		return 4
	case "major":
		return 3
	case "minor":
		return 2
	default:
		return 1
	}
}

func uniqueStrings(values []string) []string {
	unique := make([]string, 0, len(values))
	for _, value := range values {
		unique = appendUniqueString(unique, value)
	}
	sort.Strings(unique)
	return unique
}

func nonEmptyStrings(values ...string) []string {
	var result []string
	for _, value := range values {
		result = appendUniqueString(result, value)
	}
	return result
}

func appendUniqueString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if normalizedText(existing) == normalizedText(value) {
			return values
		}
	}
	return append(values, value)
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
