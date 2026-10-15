package cli

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/forge"
	"github.com/fullsend-ai/fullsend/internal/forge/gitlab"
	"github.com/fullsend-ai/fullsend/internal/repos"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

// A client without the live GitLab API (the fake) has no trigger-account
// lifecycle: install neither fails nor reports anything.
func TestMaybeConvergeGitLabTriggerAccount_SkipsNonLiveClient(t *testing.T) {
	var buf bytes.Buffer
	err := maybeConvergeGitLabTriggerAccount(context.Background(), &reposInstallConfig{}, forge.NewFakeClient(), ui.New(&buf), "g", "p", true)
	require.NoError(t, err)
	assert.Empty(t, buf.String())

	st := &repos.RepoStatus{Owner: "g", Repo: "p"}
	assert.False(t, appendGitLabTriggerAccountStatus(context.Background(), forge.NewFakeClient(), st))
	assert.Nil(t, st.GitLabTriggerAccount)
}

// A dry run reports what would happen and calls GitLab for nothing: the server
// catch-all route fails the test on any request.
func TestMaybeConvergeGitLabTriggerAccount_DryRunMakesNoRequests(t *testing.T) {
	s := newSATestServer(t)
	s.handle("/", func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s during a dry run", r.Method, r.URL.Path)
	})
	var buf bytes.Buffer
	var client forge.Client = s.client("admin-token")
	_, isLive := client.(*gitlab.LiveClient)
	require.True(t, isLive)

	err := maybeConvergeGitLabTriggerAccount(context.Background(), &reposInstallConfig{dryRun: true}, client, ui.New(&buf), "g", "p", false)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "Would converge the GitLab trigger account")
}
