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
	return evaluate(runID, target, reviewers, checks, blocking, minimumApprovals, nil, nil, now)
}

func EvaluateWithCrossExaminations(runID string, target model.Target, reviewers []model.ReviewerResult, checks []model.CheckResult, blocking []string, minimumApprovals int, crossExaminations []model.CrossExamination, now time.Time) model.Decision {
	return evaluate(runID, target, reviewers, checks, blocking, minimumApprovals, crossExaminations, nil, now)
}

// EvaluateWithCarriedFindings preserves unresolved findings from prior reviews
// of the same exact diff. They remain open until an explicit cross-examination
// records them as rejected; omission by a later reviewer is not resolution.
func EvaluateWithCarriedFindings(runID string, target model.Target, reviewers []model.ReviewerResult, checks []model.CheckResult, blocking []string, minimumApprovals int, crossExaminations []model.CrossExamination, carried []model.ConsolidatedFinding, now time.Time) model.Decision {
	return evaluate(runID, target, reviewers, checks, blocking, minimumApprovals, crossExaminations, carried, now)
}

func evaluate(runID string, target model.Target, reviewers []model.ReviewerResult, checks []model.CheckResult, blocking []string, minimumApprovals int, crossExaminations []model.CrossExamination, carried []model.ConsolidatedFinding, now time.Time) model.Decision {
	decision := model.Decision{
		SchemaVersion:  model.SchemaVersion,
		RunID:          runID,
		BaseSHA:        target.BaseSHA,
		HeadSHA:        target.HeadSHA,
		DiffHash:       target.DiffHash,
		Reviewers:      make(map[string]string, len(reviewers)),
		ReviewerErrors: make(map[string]string),
		OpenFindings: map[string]int{
			"blocker": 0,
			"major":   0,
			"minor":   0,
			"note":    0,
		},
		Checks:               make(map[string]string, len(checks)),
		CreatedAt:            now.UTC(),
		CrossExaminations:    append([]model.CrossExamination(nil), crossExaminations...),
		CarryForwardFindings: append([]model.ConsolidatedFinding{}, carried...),
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
	deferredRequiredReview := false
	explicitChangesRequested := false
	approvals := 0
	var findings []findingWithReviewer
	var carryForwardFindings []findingWithReviewer
	var residualRisks []string
	verdictNames := make(map[string][]string)
	for _, reviewer := range reviewers {
		if reviewer.Report == nil {
			if reviewer.Status == "deferred" && reviewer.FailureKind == "outcome_fixed" {
				decision.Reviewers[reviewer.Reviewer] = "deferred"
				deferredRequiredReview = true
				if reviewer.Error != "" {
					decision.ReviewerErrors[reviewer.Reviewer] = reviewer.Error
				}
				continue
			}
			decision.Reviewers[reviewer.Reviewer] = "incomplete"
			if reviewer.Error != "" {
				decision.ReviewerErrors[reviewer.Reviewer] = reviewer.Error
			}
			incomplete = true
			continue
		}
		report := reviewer.Report
		if reviewer.Status == "completed" {
			decision.Reviewers[reviewer.Reviewer] = report.Verdict
		} else {
			decision.Reviewers[reviewer.Reviewer] = "partial"
			incomplete = true
			if reviewer.Error != "" {
				decision.ReviewerErrors[reviewer.Reviewer] = reviewer.Error
			}
		}
		verdictNames[report.Verdict] = append(verdictNames[report.Verdict], reviewer.Reviewer)
		if !report.ContextComplete {
			incomplete = true
		}
		switch report.Verdict {
		case "approve":
			if countsTowardApprovalQuorum(reviewer) {
				approvals++
			}
		case "request_changes":
			hasBlockingFinding := false
			for _, finding := range report.Findings {
				if blockingSet[finding.Severity] {
					hasBlockingFinding = true
					break
				}
			}
			if !hasBlockingFinding {
				explicitChangesRequested = true
			}
		case "abstain":
			abstained = true
		}
		for _, finding := range report.Findings {
			findings = append(findings, findingWithReviewer{reviewer: reviewer.Reviewer, finding: finding})
			if reviewer.Status == "completed" {
				carryForwardFindings = append(carryForwardFindings, findingWithReviewer{reviewer: reviewer.Reviewer, finding: finding})
			}
		}
		residualRisks = append(residualRisks, report.ResidualRisks...)
	}
	consolidated := mergeCarriedFindings(consolidateFindings(findings), carried)
	decision.Findings, decision.RejectedFindings, incomplete = applyCrossExaminations(consolidated, crossExaminations, incomplete)
	durable := mergeCarriedFindings(consolidateFindings(carryForwardFindings), carried)
	decision.CarryForwardFindings, _, _ = applyCrossExaminations(durable, crossExaminations, false)
	changesRequested := explicitChangesRequested
	for _, finding := range decision.Findings {
		decision.OpenFindings[finding.Severity]++
		if blockingSet[finding.Severity] {
			changesRequested = true
		}
	}
	if crossExaminationApproval(crossExaminations) {
		approvals++
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
	case deferredRequiredReview:
		decision.State = model.StateIncomplete
		decision.Reason = "a required targeted review was deferred until earlier blocking results are resolved"
	case approvals < minimumApprovals:
		decision.State = model.StateIncomplete
		decision.Reason = fmt.Sprintf("approval quorum not met: got %d, need %d", approvals, minimumApprovals)
	case !target.Finalizable:
		decision.State = model.StateIncomplete
		decision.Reason = "working-tree reviews are advisory and cannot create an approval"
	default:
		decision.State = model.StateApproved
		nonBlocking := decision.OpenFindings["minor"] + decision.OpenFindings["note"]
		if crossExaminationApproval(crossExaminations) {
			decision.OutcomeQualifier = "cross_examined"
			decision.Reason = "approval quorum met after independent cross-examination resolved disputed blocking findings"
		} else if nonBlocking > 0 {
			decision.OutcomeQualifier = "non_blocking_findings"
			decision.Reason = fmt.Sprintf("all %d reviewers approved the exact change with %d non-blocking findings", len(reviewers), nonBlocking)
		} else {
			decision.Reason = fmt.Sprintf("all %d reviewers approved the exact change", len(reviewers))
		}
	}
	return decision
}

// A targeted security pass is required evidence for a sensitive change: any
// failure, abstention, or blocking finding still fails closed. Its scoped
// approval does not replace one of the configured full-diff reviewer approvals.
func countsTowardApprovalQuorum(reviewer model.ReviewerResult) bool {
	return !isTargetedSecurityReviewer(reviewer)
}

// BlockingCandidates returns sole-reviewer blocker/major findings that can
// change the verdict if an independent cross-examiner disproves or demotes
// them. All severities participate in consolidation before candidates are
// selected: reviewers can corroborate the same defect while disagreeing about
// its severity. Corroborated findings already satisfy the consensus burden.
func BlockingCandidates(reviewers []model.ReviewerResult) []model.ConsolidatedFinding {
	return BlockingCandidatesWithCarried(reviewers, nil)
}

// BlockingCandidatesWithCarried includes unresolved findings from prior runs
// of the same exact diff. Historical findings already corroborated by multiple
// reviewers remain open without another provider pass; sole-reviewer findings
// can be explicitly retired or confirmed by cross-examination.
func BlockingCandidatesWithCarried(reviewers []model.ReviewerResult, carried []model.ConsolidatedFinding) []model.ConsolidatedFinding {
	var findings []findingWithReviewer
	for _, reviewer := range reviewers {
		if reviewer.Status != "completed" || reviewer.Report == nil || strings.Contains(reviewer.Reviewer, "cross-examination") {
			continue
		}
		for _, finding := range reviewer.Report.Findings {
			findings = append(findings, findingWithReviewer{reviewer: reviewer.Reviewer, finding: finding})
		}
	}
	consolidated := mergeCarriedFindings(consolidateFindings(findings), carried)
	candidates := make([]model.ConsolidatedFinding, 0, len(consolidated))
	for _, finding := range consolidated {
		if (finding.Severity == "blocker" || finding.Severity == "major") && len(finding.Reviewers) == 1 && !hasReviewer(finding.Reviewers, "claude-security") {
			candidates = append(candidates, finding)
		}
	}
	return candidates
}

func isTargetedSecurityReviewer(reviewer model.ReviewerResult) bool {
	return reviewer.EscalationCause == "security_sensitive" || reviewer.Reviewer == "claude-security"
}

func hasReviewer(reviewers []string, target string) bool {
	for _, reviewer := range reviewers {
		if reviewer == target {
			return true
		}
	}
	return false
}

func applyCrossExaminations(findings []model.ConsolidatedFinding, examinations []model.CrossExamination, incomplete bool) ([]model.ConsolidatedFinding, []model.ConsolidatedFinding, bool) {
	byFinding := make(map[string]model.CrossExamination, len(examinations))
	for _, examination := range examinations {
		byFinding[examination.FindingID] = examination
	}
	open := make([]model.ConsolidatedFinding, 0, len(findings))
	var rejected []model.ConsolidatedFinding
	for _, finding := range findings {
		examination, found := byFinding[finding.ID]
		if !found {
			open = append(open, finding)
			continue
		}
		finding.OriginalSeverity = finding.Severity
		finding.Disposition = examination.Disposition
		finding.CrossExaminer = examination.Reviewer
		if examination.Status != "completed" || examination.Disposition == "uncertain" {
			incomplete = true
			open = append(open, finding)
			continue
		}
		if examination.Reachability != nil {
			finding.Reachability = examination.Reachability
		}
		switch examination.Disposition {
		case "confirmed":
			if examination.EffectiveSeverity != "" {
				finding.Severity = examination.EffectiveSeverity
			}
			finding.Reviewers = appendUniqueString(finding.Reviewers, examination.Reviewer)
			open = append(open, finding)
		case "demoted":
			finding.Severity = examination.EffectiveSeverity
			finding.Reviewers = appendUniqueString(finding.Reviewers, examination.Reviewer)
			open = append(open, finding)
		case "disproved":
			rejected = append(rejected, finding)
		default:
			incomplete = true
			open = append(open, finding)
		}
	}
	return open, rejected, incomplete
}

func crossExaminationApproval(examinations []model.CrossExamination) bool {
	if len(examinations) == 0 {
		return false
	}
	for _, examination := range examinations {
		if examination.Status != "completed" || (examination.Disposition != "demoted" && examination.Disposition != "disproved") {
			return false
		}
	}
	return true
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
				SourceIDs:    nonEmptyStrings(item.finding.ID),
				Reachability: item.finding.Reachability,
			})
			continue
		}
		merged := &consolidated[match]
		mergedRank := severityRank(merged.Severity)
		candidateRank := severityRank(item.finding.Severity)
		if candidateRank > mergedRank {
			merged.Severity = item.finding.Severity
			merged.Claim = item.finding.Claim
			merged.Reachability = item.finding.Reachability
		}
		if item.finding.Confidence > merged.Confidence {
			merged.Confidence = item.finding.Confidence
			if candidateRank == mergedRank {
				merged.Claim = item.finding.Claim
				merged.Reachability = item.finding.Reachability
			}
		}
		if merged.Reachability == nil && item.finding.Reachability != nil {
			merged.Reachability = item.finding.Reachability
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

func mergeCarriedFindings(current, carried []model.ConsolidatedFinding) []model.ConsolidatedFinding {
	merged := append([]model.ConsolidatedFinding(nil), current...)
	for _, prior := range carried {
		match := -1
		candidate := model.Finding{
			ID: prior.ID, Severity: prior.Severity, Confidence: prior.Confidence,
			File: prior.File, Line: prior.Line, Claim: prior.Claim,
			Evidence: strings.Join(prior.Evidence, " "), SuggestedFix: strings.Join(prior.SuggestedFixes, " "),
			Reachability: prior.Reachability,
		}
		for index := range merged {
			if equivalentFinding(merged[index], candidate) {
				match = index
				break
			}
		}
		if match < 0 {
			merged = append(merged, prior)
			continue
		}
		finding := &merged[match]
		if severityRank(prior.Severity) > severityRank(finding.Severity) {
			finding.Severity = prior.Severity
			finding.Claim = prior.Claim
			finding.Reachability = prior.Reachability
		}
		if prior.Confidence > finding.Confidence {
			finding.Confidence = prior.Confidence
		}
		if finding.Reachability == nil && prior.Reachability != nil {
			finding.Reachability = prior.Reachability
		}
		for _, evidence := range prior.Evidence {
			finding.Evidence = appendUniqueString(finding.Evidence, evidence)
		}
		for _, fix := range prior.SuggestedFixes {
			finding.SuggestedFixes = appendUniqueString(finding.SuggestedFixes, fix)
		}
		for _, reviewer := range prior.Reviewers {
			finding.Reviewers = appendUniqueString(finding.Reviewers, reviewer)
		}
		for _, sourceID := range prior.SourceIDs {
			finding.SourceIDs = appendUniqueString(finding.SourceIDs, sourceID)
		}
		for _, priorRunID := range prior.CarriedFromRunIDs {
			finding.CarriedFromRunIDs = appendUniqueString(finding.CarriedFromRunIDs, priorRunID)
		}
		if prior.ID != "" && prior.ID != finding.ID {
			finding.HistoricalFindingIDs = appendUniqueString(finding.HistoricalFindingIDs, prior.ID)
		}
		for _, historicalID := range prior.HistoricalFindingIDs {
			finding.HistoricalFindingIDs = appendUniqueString(finding.HistoricalFindingIDs, historicalID)
		}
	}
	for index := range merged {
		sort.Strings(merged[index].Reviewers)
		sort.Strings(merged[index].SourceIDs)
		sort.Strings(merged[index].CarriedFromRunIDs)
		sort.Strings(merged[index].HistoricalFindingIDs)
	}
	sort.Slice(merged, func(i, j int) bool {
		if severityRank(merged[i].Severity) != severityRank(merged[j].Severity) {
			return severityRank(merged[i].Severity) > severityRank(merged[j].Severity)
		}
		if merged[i].File != merged[j].File {
			return merged[i].File < merged[j].File
		}
		return merged[i].Line < merged[j].Line
	})
	return merged
}

func equivalentFinding(existing model.ConsolidatedFinding, candidate model.Finding) bool {
	if strings.EqualFold(strings.TrimSpace(existing.File), strings.TrimSpace(candidate.File)) == false {
		return false
	}
	left := findingTokens(existing.Claim)
	right := findingTokens(candidate.Claim)
	claimSimilarity, shared := tokenSimilarity(left, right)
	lineDistance := abs(existing.Line - candidate.Line)
	linesKnown := existing.Line > 0 && candidate.Line > 0
	linesNearby := linesKnown && lineDistance <= 3

	// Reviewers commonly anchor the same defect to adjacent lines. Preserve a
	// deliberately conservative fast path for those findings.
	if (!linesKnown || linesNearby) && (claimSimilarity >= 0.45 || (lineDistance == 0 && shared >= 4 && claimSimilarity >= 0.25) || (lineDistance <= 3 && shared >= 4 && claimSimilarity >= 0.33)) {
		return true
	}
	if !linesKnown || linesNearby {
		for _, evidence := range existing.Evidence {
			if normalizedText(evidence) != "" && normalizedText(evidence) == normalizedText(candidate.Evidence) {
				return true
			}
			evidenceSimilarity, evidenceShared := tokenSimilarity(findingTokens(evidence), findingTokens(candidate.Evidence))
			if evidenceShared >= 4 && evidenceSimilarity >= 0.5 {
				return true
			}
		}
		for _, fix := range existing.SuggestedFixes {
			fixSimilarity, fixShared := tokenSimilarity(findingTokens(fix), findingTokens(candidate.SuggestedFix))
			if lineDistance == 0 && fixShared >= 3 && fixSimilarity >= 0.5 {
				return true
			}
		}
	}

	// A defect can span a source and sink hundreds of lines apart, and two
	// reviewers may reasonably anchor it to opposite ends. For distant anchors,
	// require agreement in both the concise claim and the richer supporting
	// context. This catches paraphrases of the same causal chain without merging
	// unrelated issues merely because they share a file or generic vocabulary.
	contextSimilarity, contextShared := tokenSimilarity(consolidatedFindingTokens(existing), findingContextTokens(candidate))
	if shared >= 5 && claimSimilarity >= 0.32 && contextShared >= 10 && contextSimilarity >= 0.4 {
		return true
	}
	return false
}

func consolidatedFindingTokens(finding model.ConsolidatedFinding) map[string]bool {
	tokens := findingTokens(finding.Claim)
	for _, evidence := range finding.Evidence {
		mergeTokens(tokens, findingTokens(evidence))
	}
	for _, fix := range finding.SuggestedFixes {
		mergeTokens(tokens, findingTokens(fix))
	}
	mergeTokens(tokens, reachabilityTokens(finding.Reachability))
	return tokens
}

func findingContextTokens(finding model.Finding) map[string]bool {
	tokens := findingTokens(finding.Claim)
	mergeTokens(tokens, findingTokens(finding.Evidence))
	mergeTokens(tokens, findingTokens(finding.SuggestedFix))
	mergeTokens(tokens, reachabilityTokens(finding.Reachability))
	return tokens
}

func reachabilityTokens(reachability *model.Reachability) map[string]bool {
	tokens := make(map[string]bool)
	if reachability == nil {
		return tokens
	}
	mergeTokens(tokens, findingTokens(reachability.Trigger))
	mergeTokens(tokens, findingTokens(reachability.Impact))
	for _, step := range reachability.Path {
		mergeTokens(tokens, findingTokens(step))
	}
	for _, precondition := range reachability.Preconditions {
		mergeTokens(tokens, findingTokens(precondition))
	}
	return tokens
}

func mergeTokens(destination, source map[string]bool) {
	for token := range source {
		destination[token] = true
	}
}

func findingTokens(value string) map[string]bool {
	stop := map[string]bool{
		"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true,
		"by": true, "can": true, "could": true, "does": true, "for": true, "from": true,
		"in": true, "is": true, "it": true, "may": true, "new": true, "not": true, "of": true,
		"on": true, "only": true, "or": true, "that": true, "the": true, "this": true, "to": true,
		"when": true, "where": true, "which": true, "with": true, "without": true,
	}
	words := strings.FieldsFunc(strings.ToLower(value), func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsDigit(character)
	})
	tokens := make(map[string]bool)
	for _, word := range words {
		word = normalizeToken(stemToken(word))
		if len(word) > 1 && !stop[word] {
			tokens[word] = true
		}
	}
	return tokens
}

func normalizeToken(word string) string {
	switch word {
	case "assert", "assertion", "check", "postcondition", "verify", "verification":
		return "assert"
	case "delet", "deleted", "undelet":
		return "delete"
	case "exist", "left", "present", "retain", "remain":
		return "remain"
	case "namespace", "namespac":
		return "namespace"
	default:
		return word
	}
}

func stemToken(word string) string {
	for _, suffix := range []string{"ization", "ations", "ation", "ments", "ment", "ingly", "edly", "ing", "ied", "ed", "es", "s"} {
		if strings.HasSuffix(word, suffix) && len(word)-len(suffix) >= 4 {
			stem := strings.TrimSuffix(word, suffix)
			if suffix == "ied" {
				stem += "y"
			}
			return stem
		}
	}
	return word
}

func tokenSimilarity(left, right map[string]bool) (float64, int) {
	if len(left) == 0 || len(right) == 0 {
		return 0, 0
	}
	intersection := 0
	for token := range left {
		if right[token] {
			intersection++
		}
	}
	smaller := len(left)
	if len(right) < smaller {
		smaller = len(right)
	}
	containment := float64(intersection) / float64(smaller)
	return max(jaccard(left, right), containment), intersection
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
