package worker

import (
	"context"
	"encoding/json"

	"durable-jobs/internal/campaign"
	"durable-jobs/internal/jobs"
)

// CampaignProcessor adapts the pure migration to the worker's processing port.
// It computes a result only: Finish atomically stores that result with the status.
type CampaignProcessor struct{}

func (CampaignProcessor) Process(ctx context.Context, job jobs.Job) (jobs.Outcome, error) {
	if err := ctx.Err(); err != nil {
		return jobs.Outcome{}, err
	}
	if job.Type != "migrate_campaign" {
		return jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.UnsupportedJobType}, nil
	}
	result, err := campaign.Migrate(job.Payload)
	if err != nil {
		return jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.InvalidCampaign}, nil
	}
	encoded, err := json.Marshal(result)
	return jobs.Outcome{Status: jobs.Succeeded, Result: encoded}, err
}
