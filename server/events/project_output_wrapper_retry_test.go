// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"errors"
	"testing"

	. "github.com/petergtz/pegomock/v4"
	"github.com/runatlantis/atlantis/server/events"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/mocks"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/logging"
)

// The fork publishes a project's success status as soon as the project
// finishes. When that request is lost, the deferred publisher must deliver it,
// otherwise the check stays pending forever.
func TestProjectOutputWrapper_RetriesUndeliveredSuccessInDeferredPublish(t *testing.T) {
	cases := []struct {
		name    string
		cmd     command.Name
		output  command.ProjectCommandOutput
		publish func(r *events.ProjectOutputWrapper, cmds []command.ProjectContext, res command.Result)
	}{
		{
			name:   "plan",
			cmd:    command.Plan,
			output: command.ProjectCommandOutput{PlanSuccess: &models.PlanSuccess{TerraformOutput: "No changes."}},
			publish: func(r *events.ProjectOutputWrapper, cmds []command.ProjectContext, res command.Result) {
				r.PublishDeferredPlanStatuses(cmds, res, models.SuccessCommitStatus)
			},
		},
		{
			name:   "apply",
			cmd:    command.Apply,
			output: command.ProjectCommandOutput{ApplySuccess: "apply complete"},
			publish: func(r *events.ProjectOutputWrapper, cmds []command.ProjectContext, res command.Result) {
				r.PublishDeferredApplyStatuses(cmds, res, models.SuccessCommitStatus)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			RegisterMockTestingT(t)
			ctx := command.ProjectContext{
				Log:         logging.NewNoopLogger(t),
				CommandName: c.cmd,
				Workspace:   "default",
				RepoRelDir:  "dir",
			}
			output := c.output
			mockJobURLSetter := mocks.NewMockJobURLSetter()
			mockProjectCommandRunner := mocks.NewMockProjectCommandRunner()
			runner := &events.ProjectOutputWrapper{
				JobURLSetter:         mockJobURLSetter,
				JobMessageSender:     mocks.NewMockJobMessageSender(),
				ProjectCommandRunner: mockProjectCommandRunner,
			}
			When(mockProjectCommandRunner.Plan(Any[command.ProjectContext]())).ThenReturn(output)
			When(mockProjectCommandRunner.Apply(Any[command.ProjectContext]())).ThenReturn(output)
			// The immediate success status is lost, the retry goes through.
			When(mockJobURLSetter.SetJobURLWithStatus(ctx, c.cmd, models.SuccessCommitStatus, &output)).
				ThenReturn(errors.New("stream error")).
				ThenReturn(nil)

			if c.cmd == command.Plan {
				runner.Plan(ctx)
			} else {
				runner.Apply(ctx)
			}
			mockJobURLSetter.VerifyWasCalled(Once()).SetJobURLWithStatus(ctx, c.cmd, models.SuccessCommitStatus, &output)

			result := command.Result{ProjectResults: []command.ProjectResult{{
				ProjectCommandOutput: output,
				Command:              c.cmd,
				RepoRelDir:           ctx.RepoRelDir,
				Workspace:            ctx.Workspace,
			}}}
			cmds := []command.ProjectContext{ctx}
			c.publish(runner, cmds, result)
			mockJobURLSetter.VerifyWasCalled(Twice()).SetJobURLWithStatus(ctx, c.cmd, models.SuccessCommitStatus, &output)

			// Delivered now, so another deferred publish does not repeat it.
			c.publish(runner, cmds, result)
			mockJobURLSetter.VerifyWasCalled(Twice()).SetJobURLWithStatus(ctx, c.cmd, models.SuccessCommitStatus, &output)
		})
	}
}
