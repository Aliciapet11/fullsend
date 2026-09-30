package steps

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/cucumber/godog"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/statuscomment"
	"github.com/fullsend-ai/fullsend/pkg/behaviourtest/world"
)

func registerStatusCommentSteps(sc *godog.ScenarioContext) {
	sc.Step(`^status notification comments are disabled$`, func(ctx context.Context) (context.Context, error) {
		return ctx, givenCommentsDisabled(world.FromContext(ctx))
	})
	sc.Step(`^the (issue|pull request) has a status (?:comment|marker)$`, func(ctx context.Context, target string) (context.Context, error) {
		return ctx, thenTargetHasStatusComment(world.FromContext(ctx), target)
	})
	sc.Step(`^the (issue|pull request) does not have a status (?:comment|marker)$`, func(ctx context.Context, target string) (context.Context, error) {
		return ctx, thenTargetDoesNotHaveStatusComment(world.FromContext(ctx), target)
	})
}

// givenCommentsDisabled sets status_notifications.comment.start and
// .completion to "disabled" in the enrolled repo's config.yaml.
func givenCommentsDisabled(w *world.World) error {
	cfgPath := filepath.Join(".fullsend", "config.yaml")
	cfgData, err := w.SCM.GetFileContent(context.Background(), w.Org, w.RepoName, cfgPath)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	cfg, err := config.ParsePerRepoConfigWriter(cfgData)
	if err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	sn := cfg.StatusNotifications()
	if sn == nil {
		sn = &config.StatusNotificationConfig{}
	}
	sn.Comment = config.CommentNotificationConfig{
		Start:      "disabled",
		Completion: "disabled",
	}
	cfg.SetStatusNotifications(sn)
	merged, err := cfg.Marshal()
	if err != nil {
		return err
	}
	if err := w.SCM.CommitFile(context.Background(), w.Org, w.RepoName, cfgPath, "behaviour: disable status comment notifications", merged); err != nil {
		return fmt.Errorf("updating config: %w", err)
	}
	return nil
}

// ResetStatusCommentNotifications removes or resets status_notifications.comment
// from the enrolled repo's config.yaml. Exported so CleanupScenario can
// call it during scenario teardown.
func ResetStatusCommentNotifications(w *world.World) error {
	cfgPath := filepath.Join(".fullsend", "config.yaml")
	cfgData, err := w.SCM.GetFileContent(context.Background(), w.Org, w.RepoName, cfgPath)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	cfg, err := config.ParsePerRepoConfigWriter(cfgData)
	if err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	cfg.SetStatusNotifications(nil)
	merged, err := cfg.Marshal()
	if err != nil {
		return err
	}
	if err := w.SCM.CommitFile(context.Background(), w.Org, w.RepoName, cfgPath, "behaviour: reset status comment notifications", merged); err != nil {
		return fmt.Errorf("updating config: %w", err)
	}
	return nil
}

// commentsDisabledInConfig checks if status_notifications.comment is
// set to disabled in the repo config. Used by cleanup to decide whether to reset.
func commentsDisabledInConfig(w *world.World) bool {
	if w.SCM == nil || w.Org == "" || w.RepoName == "" {
		return false
	}
	cfgPath := filepath.Join(".fullsend", "config.yaml")
	cfgData, err := w.SCM.GetFileContent(context.Background(), w.Org, w.RepoName, cfgPath)
	if err != nil {
		return false
	}
	cfg, err := config.ParsePerRepoConfigWriter(cfgData)
	if err != nil {
		return false
	}
	sn := cfg.StatusNotifications()
	if sn == nil {
		return false
	}
	return sn.Comment.Start == "disabled" || sn.Comment.Completion == "disabled"
}

func targetNumber(w *world.World, target string) (int, error) {
	switch target {
	case "issue":
		if w.IssueNumber == 0 {
			return 0, fmt.Errorf("no issue created")
		}
		return w.IssueNumber, nil
	case "pull request":
		if w.PRNumber == 0 {
			return 0, fmt.Errorf("no pull request opened")
		}
		return w.PRNumber, nil
	default:
		return 0, fmt.Errorf("unsupported target %q (must be \"issue\" or \"pull request\")", target)
	}
}

func thenTargetHasStatusComment(w *world.World, target string) error {
	num, err := targetNumber(w, target)
	if err != nil {
		return err
	}
	comments, err := w.SCM.ListComments(context.Background(), w.RepoOwner, w.RepoName, num)
	if err != nil {
		return fmt.Errorf("listing comments on %s #%d: %w", target, num, err)
	}
	for _, c := range comments {
		if statuscomment.IsStatusComment(c.Body) {
			return nil
		}
	}
	return fmt.Errorf("%s #%d does not have a status comment (%d comments checked)", target, num, len(comments))
}

func thenTargetDoesNotHaveStatusComment(w *world.World, target string) error {
	num, err := targetNumber(w, target)
	if err != nil {
		return err
	}
	comments, err := w.SCM.ListComments(context.Background(), w.RepoOwner, w.RepoName, num)
	if err != nil {
		return fmt.Errorf("listing comments on %s #%d: %w", target, num, err)
	}
	for _, c := range comments {
		if statuscomment.IsStatusComment(c.Body) {
			return fmt.Errorf("%s #%d unexpectedly has a status comment: %s", target, num, c.Body)
		}
	}
	return nil
}
