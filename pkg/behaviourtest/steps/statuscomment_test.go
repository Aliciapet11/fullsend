package steps

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/forge"
	"github.com/fullsend-ai/fullsend/pkg/behaviourtest/drivers/scm"
	"github.com/fullsend-ai/fullsend/pkg/behaviourtest/world"
)

type fakeStatusCommentSCM struct {
	scm.Driver

	fileContent    map[string][]byte
	committedFiles map[string][]byte
	getFileErr     error
	commitFileErr  error

	comments []forge.IssueComment
	listErr  error
}

func (f *fakeStatusCommentSCM) GetFileContent(_ context.Context, _, _, path string) ([]byte, error) {
	if f.getFileErr != nil {
		return nil, f.getFileErr
	}
	if content, ok := f.fileContent[path]; ok {
		return content, nil
	}
	return nil, errors.New("file not found")
}

func (f *fakeStatusCommentSCM) CommitFile(_ context.Context, _, _, path, _ string, content []byte) error {
	if f.commitFileErr != nil {
		return f.commitFileErr
	}
	if f.committedFiles == nil {
		f.committedFiles = make(map[string][]byte)
	}
	f.committedFiles[path] = content
	if f.fileContent == nil {
		f.fileContent = make(map[string][]byte)
	}
	f.fileContent[path] = content
	return nil
}

func (f *fakeStatusCommentSCM) ListComments(_ context.Context, _, _ string, _ int) ([]forge.IssueComment, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.comments, nil
}

func TestGivenCommentsDisabled(t *testing.T) {
	t.Run("disables comments in existing empty config", func(t *testing.T) {
		s := &fakeStatusCommentSCM{
			fileContent: map[string][]byte{
				".fullsend/config.yaml": []byte("version: 1\n"),
			},
		}
		w := &world.World{Org: "org", RepoName: "repo", SCM: s}
		require.NoError(t, givenCommentsDisabled(w))

		assert.Contains(t, string(s.committedFiles[".fullsend/config.yaml"]), "status_notifications:")
		assert.Contains(t, string(s.committedFiles[".fullsend/config.yaml"]), "start: disabled")
		assert.Contains(t, string(s.committedFiles[".fullsend/config.yaml"]), "completion: disabled")
		assert.True(t, commentsDisabledInConfig(w))
	})

	t.Run("read error propagates", func(t *testing.T) {
		s := &fakeStatusCommentSCM{getFileErr: errors.New("read failure")}
		w := &world.World{Org: "org", RepoName: "repo", SCM: s}
		err := givenCommentsDisabled(w)
		require.ErrorContains(t, err, "reading config: read failure")
	})

	t.Run("commit error propagates", func(t *testing.T) {
		s := &fakeStatusCommentSCM{
			fileContent: map[string][]byte{
				".fullsend/config.yaml": []byte("version: 1\n"),
			},
			commitFileErr: errors.New("commit failure"),
		}
		w := &world.World{Org: "org", RepoName: "repo", SCM: s}
		err := givenCommentsDisabled(w)
		require.ErrorContains(t, err, "updating config: commit failure")
	})
}

func TestResetStatusCommentNotifications(t *testing.T) {
	t.Run("clears notifications", func(t *testing.T) {
		s := &fakeStatusCommentSCM{
			fileContent: map[string][]byte{
				".fullsend/config.yaml": []byte("version: 1\nstatus_notifications:\n  comment:\n    start: disabled\n    completion: disabled\n"),
			},
		}
		w := &world.World{Org: "org", RepoName: "repo", SCM: s}
		assert.True(t, commentsDisabledInConfig(w))

		require.NoError(t, ResetStatusCommentNotifications(w))
		assert.False(t, commentsDisabledInConfig(w))
	})

	t.Run("read error propagates", func(t *testing.T) {
		s := &fakeStatusCommentSCM{getFileErr: errors.New("read failure")}
		w := &world.World{Org: "org", RepoName: "repo", SCM: s}
		err := ResetStatusCommentNotifications(w)
		require.ErrorContains(t, err, "reading config: read failure")
	})

	t.Run("commit error propagates", func(t *testing.T) {
		s := &fakeStatusCommentSCM{
			fileContent: map[string][]byte{
				".fullsend/config.yaml": []byte("version: 1\n"),
			},
			commitFileErr: errors.New("commit failure"),
		}
		w := &world.World{Org: "org", RepoName: "repo", SCM: s}
		err := ResetStatusCommentNotifications(w)
		require.ErrorContains(t, err, "updating config: commit failure")
	})
}

func TestCommentsDisabledInConfig(t *testing.T) {
	assert.False(t, commentsDisabledInConfig(&world.World{}))

	w := &world.World{Org: "org", RepoName: "repo", SCM: &fakeStatusCommentSCM{getFileErr: errors.New("not found")}}
	assert.False(t, commentsDisabledInConfig(w))

	s := &fakeStatusCommentSCM{
		fileContent: map[string][]byte{
			".fullsend/config.yaml": []byte("version: 1\nstatus_notifications:\n  comment:\n    start: disabled\n"),
		},
	}
	w = &world.World{Org: "org", RepoName: "repo", SCM: s}
	assert.True(t, commentsDisabledInConfig(w))
}

func TestThenTargetHasStatusComment(t *testing.T) {
	statusComment := "<!-- fullsend:agent-status:run-123 -->\n🤖 Agent · Started 1:00 PM UTC"
	plainComment := "Normal discussion comment"

	t.Run("found on issue", func(t *testing.T) {
		s := &fakeStatusCommentSCM{
			comments: []forge.IssueComment{{Body: plainComment}, {Body: statusComment}},
		}
		w := &world.World{IssueNumber: 42, RepoOwner: "org", RepoName: "repo", SCM: s}
		require.NoError(t, thenTargetHasStatusComment(w, "issue"))
	})

	t.Run("found on pull request", func(t *testing.T) {
		s := &fakeStatusCommentSCM{comments: []forge.IssueComment{{Body: statusComment}}}
		w := &world.World{PRNumber: 10, RepoOwner: "org", RepoName: "repo", SCM: s}
		require.NoError(t, thenTargetHasStatusComment(w, "pull request"))
	})

	t.Run("not found", func(t *testing.T) {
		s := &fakeStatusCommentSCM{
			comments: []forge.IssueComment{{Body: plainComment}},
		}
		w := &world.World{IssueNumber: 42, RepoOwner: "org", RepoName: "repo", SCM: s}
		err := thenTargetHasStatusComment(w, "issue")
		require.ErrorContains(t, err, "issue #42 does not have a status comment (1 comments checked)")
	})

	t.Run("missing issue number", func(t *testing.T) {
		w := &world.World{RepoOwner: "org", RepoName: "repo", SCM: &fakeStatusCommentSCM{}}
		err := thenTargetHasStatusComment(w, "issue")
		require.ErrorContains(t, err, "no issue created")
	})

	t.Run("missing PR number", func(t *testing.T) {
		w := &world.World{RepoOwner: "org", RepoName: "repo", SCM: &fakeStatusCommentSCM{}}
		err := thenTargetHasStatusComment(w, "pull request")
		require.ErrorContains(t, err, "no pull request opened")
	})

	t.Run("invalid target", func(t *testing.T) {
		w := &world.World{RepoOwner: "org", RepoName: "repo", SCM: &fakeStatusCommentSCM{}}
		err := thenTargetHasStatusComment(w, "commit")
		require.ErrorContains(t, err, "unsupported target \"commit\"")
	})

	t.Run("list error propagates", func(t *testing.T) {
		s := &fakeStatusCommentSCM{listErr: errors.New("api rate limit")}
		w := &world.World{IssueNumber: 42, RepoOwner: "org", RepoName: "repo", SCM: s}
		err := thenTargetHasStatusComment(w, "issue")
		require.ErrorContains(t, err, "listing comments on issue #42: api rate limit")
	})
}

func TestThenTargetDoesNotHaveStatusComment(t *testing.T) {
	statusComment := "<!-- fullsend:agent-status:run-123 -->\n🤖 Agent · Started 1:00 PM UTC"
	plainComment := "Normal discussion comment"

	t.Run("no status comment exists", func(t *testing.T) {
		s := &fakeStatusCommentSCM{
			comments: []forge.IssueComment{{Body: plainComment}},
		}
		w := &world.World{IssueNumber: 42, RepoOwner: "org", RepoName: "repo", SCM: s}
		require.NoError(t, thenTargetDoesNotHaveStatusComment(w, "issue"))
	})

	t.Run("empty comments list passes", func(t *testing.T) {
		s := &fakeStatusCommentSCM{comments: nil}
		w := &world.World{PRNumber: 15, RepoOwner: "org", RepoName: "repo", SCM: s}
		require.NoError(t, thenTargetDoesNotHaveStatusComment(w, "pull request"))
	})

	t.Run("unexpectedly has status comment", func(t *testing.T) {
		s := &fakeStatusCommentSCM{
			comments: []forge.IssueComment{{Body: plainComment}, {Body: statusComment}},
		}
		w := &world.World{IssueNumber: 42, RepoOwner: "org", RepoName: "repo", SCM: s}
		err := thenTargetDoesNotHaveStatusComment(w, "issue")
		require.ErrorContains(t, err, "issue #42 unexpectedly has a status comment")
	})

	t.Run("missing target entity", func(t *testing.T) {
		w := &world.World{RepoOwner: "org", RepoName: "repo", SCM: &fakeStatusCommentSCM{}}
		err := thenTargetDoesNotHaveStatusComment(w, "issue")
		require.ErrorContains(t, err, "no issue created")
	})

	t.Run("list error propagates", func(t *testing.T) {
		s := &fakeStatusCommentSCM{listErr: errors.New("network down")}
		w := &world.World{IssueNumber: 42, RepoOwner: "org", RepoName: "repo", SCM: s}
		err := thenTargetDoesNotHaveStatusComment(w, "issue")
		require.ErrorContains(t, err, "listing comments on issue #42: network down")
	})
}
