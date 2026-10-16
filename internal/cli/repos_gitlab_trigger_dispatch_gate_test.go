package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/fullsend-ai/fullsend/internal/forge"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

func TestReportGitLabTriggerDispatchGate_DeferredReportsEveryReason(t *testing.T) {
	// A legacy repository: its managed schedule still selects the poll mode
	// with a pipeline variable, overrides are allowed, and no trigger account
	// is installed. The report is informational and changes nothing.
	fc := forge.NewFakeClient()
	fc.PipelineSchedules["acme/widgets"] = []forge.PipelineSchedule{
		{ID: 7, Description: "fullsend slash poll", Variables: map[string]string{forge.VarPollMode: "slash"}},
	}
	fc.PipelineVarOverrideRoles["acme/widgets"] = forge.PipelineVarOverrideDeveloper

	var buf bytes.Buffer
	reportGitLabTriggerDispatchGate(context.Background(), fc, nil, ui.New(&buf), "acme", "widgets", "main")

	out := buf.String()
	assert.Contains(t, out, "[acme/widgets] GitLab trigger dispatch activation deferred")
	assert.Contains(t, out, "managed schedule 7")
	assert.Contains(t, out, `ci_pipeline_variables_minimum_override_role is "developer", not no_one_allowed`)
	assert.Contains(t, out, "the trigger account is not-installed")
	assert.Contains(t, out, "Polling stays the dispatch path")
	assert.Equal(t, "slash", fc.PipelineSchedules["acme/widgets"][0].Variables[forge.VarPollMode])
	assert.Equal(t, forge.PipelineVarOverrideDeveloper, fc.PipelineVarOverrideRoles["acme/widgets"])
}

func TestReportGitLabTriggerDispatchGate_PrerequisitesVerified(t *testing.T) {
	// A ready trigger account: a ledger in phase ready with a published,
	// masked, protected token sink, under no_one_allowed and variable-free
	// managed schedules.
	fc := forge.NewFakeClient()
	fc.PipelineVarOverrideRoles["acme/widgets"] = forge.PipelineVarOverrideNoOneAllowed
	fc.PipelineSchedules["acme/widgets"] = []forge.PipelineSchedule{{ID: 7, Description: "fullsend slash poll"}}
	fc.VariableValues["acme/widgets/"+forge.VarGitLabTriggerAccount] = `{"version":1,"phase":"ready","nonce":"0123456789abcdef","branch":"main","account_id":5,"trigger_token_id":9,"publish_started":true,"trigger_published":true}`
	if fc.Secrets == nil {
		fc.Secrets = map[string]bool{}
	}
	fc.Secrets["acme/widgets/"+forge.SecretGitLabTriggerAccountToken] = true

	var buf bytes.Buffer
	reportGitLabTriggerDispatchGate(context.Background(), fc, nil, ui.New(&buf), "acme", "widgets", "main")

	out := buf.String()
	if !assert.Contains(t, out, "GitLab trigger dispatch prerequisites verified") {
		return
	}
	assert.Contains(t, out, "the dispatch route is not yet activated")
	assert.NotContains(t, out, "deferred")
}
