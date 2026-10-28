// Package harness reports named reliability checks using the existing Go tests.
// It does not implement a second worker or add production failure switches.
package harness

import (
	"fmt"
	"strings"
)

type Scenario struct {
	ID       string `json:"id"`
	Test     string `json:"test"`
	Expected string `json:"expected"`
}

// Tests are explicit, so a rename or skipped test cannot silently turn green.
func Scenarios() []Scenario {
	return []Scenario{
		{"campaign-fixtures", "TestCampaignFixtureMatrixIntegration", "Valid campaigns produce exact expected JSON; invalid campaigns fail once without retry; HTTP history agrees."},
		{"duplicate-submissions", "TestConcurrentIdempotentHTTPSubmissionsIntegration", "Concurrent matching submissions share one job; conflicting input is rejected."},
		{"retry-then-success", "TestWorkerRetryThenSuccessIntegration", "An explicitly transient failure retries and succeeds on attempt two."},
		{"retry-exhaustion", "TestRetryBudgetAndHistoryIntegration", "Retry delays are enforced and the third failed attempt is terminal; no fourth claim."},
		{"permanent-failures", "TestPermanentFailuresDoNotRetryIntegration", "Permanent processing failures become terminal after one attempt."},
		{"crash-recovery", "TestKilledWorkerRecoveredByNewProcessIntegration", "A killed owned process leaves a claim; fresh CLI workers recover and complete attempt two after real expiry/backoff."},
		{"concurrent-recovery", "TestRecoveryBatchConcurrencyAndLocksIntegration", "Concurrent recovery has one outcome per abandoned claim, skips locked rows and preserves fresh leases."},
		{"stale-worker", "TestExpiredClaimRecoveryFencesOldWorkerIntegration", "Expired and old claim tokens cannot publish results before or after a new claim."},
		{"atomic-history", "TestAttemptHistoryCommitFailuresRollBackTogetherIntegration", "Injected claim/finish/recovery commit failures roll back job and attempt changes together."},
		{"schema-upgrade", "TestRetryMigrationBackfillsWithoutReopeningJobsIntegration", "Migration preserves old jobs and known history without reopening terminal jobs."},
	}
}

func Select(value string) ([]Scenario, error) {
	all := Scenarios()
	if value == "all" {
		return all, nil
	}
	byID := make(map[string]Scenario, len(all))
	for _, item := range all {
		byID[item.ID] = item
	}
	var selected []Scenario
	seen := map[string]bool{}
	for _, id := range strings.Split(value, ",") {
		id = strings.TrimSpace(id)
		item, exists := byID[id]
		if !exists || seen[id] {
			return nil, fmt.Errorf("unknown or repeated scenario %q; use -list", id)
		}
		seen[id] = true
		selected = append(selected, item)
	}
	return selected, nil
}
