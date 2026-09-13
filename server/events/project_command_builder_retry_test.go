// Copyright 2026 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/petergtz/pegomock/v4"
	"github.com/runatlantis/atlantis/server/core/config"
	"github.com/runatlantis/atlantis/server/core/config/valid"
	"github.com/runatlantis/atlantis/server/core/runtime"
	tfclientmocks "github.com/runatlantis/atlantis/server/core/terraform/tfclient/mocks"
	"github.com/runatlantis/atlantis/server/events"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/mocks"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/logging"
	"github.com/runatlantis/atlantis/server/metrics/metricstest"
	. "github.com/runatlantis/atlantis/testing"
)

func TestDefaultProjectCommandBuilder_BuildMultiApplyRetry(t *testing.T) {
	for _, tc := range []struct {
		name          string
		applied       int
		failed        int
		pending       int
		head          string
		base          string
		missingStatus bool
		missingPlan   bool
		expErr        string
	}{
		{name: "retry failed and pending", applied: 1, failed: 1, pending: 1},
		{name: "1500 projects with 500 applied", applied: 500, failed: 500, pending: 500},
		{name: "all applied", applied: 2},
		{name: "stale head still rejected", applied: 1, failed: 1, head: "old-head", expErr: "from commit"},
		{name: "all applied stale head still rejected", applied: 2, head: "old-head", expErr: "from commit"},
		{name: "retargeted base still rejected", applied: 2, base: "release", expErr: "base branch"},
		{name: "unrecorded file still rejected", applied: 1, missingStatus: true, expErr: "no matching plan status"},
		{name: "missing retry plan still rejected", applied: 1, failed: 1, missingPlan: true, expErr: "plan file is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			RegisterMockTestingT(t)
			logger := logging.NewNoopLogger(t)
			scope := metricstest.NewLoggingScope(t, logger, "atlantis")
			pull := models.PullRequest{HeadCommit: "current-head", BaseBranch: "main"}
			pullStatus := &models.PullStatus{Pull: pull}
			if tc.head != "" {
				pullStatus.Pull.HeadCommit = tc.head
			}
			if tc.base != "" {
				pullStatus.Pull.BaseBranch = tc.base
			}

			projects := make(map[string]any)
			for i := 0; i < tc.applied+tc.failed+tc.pending; i++ {
				dir := fmt.Sprintf("project-%04d", i)
				status := models.PlannedPlanStatus
				if i < tc.applied {
					status = models.AppliedPlanStatus
				} else if i < tc.applied+tc.failed {
					status = models.ErroredApplyStatus
				}
				pullStatus.Projects = append(pullStatus.Projects, models.ProjectStatus{
					RepoRelDir: dir, Workspace: "default", Status: status,
				})
				files := map[string]any{"main.tf": nil}
				if !tc.missingPlan || status == models.AppliedPlanStatus {
					files["default.tfplan"] = "saved plan"
				}
				projects[dir] = files
			}
			if tc.missingStatus {
				projects["unrecorded"] = map[string]any{"main.tf": nil, "default.tfplan": "unrecorded plan"}
			}
			pullDir := DirStructure(t, map[string]any{"default": projects})
			repoDir := filepath.Join(pullDir, "default")
			runCmd(t, repoDir, "git", "init")
			workingDir := mocks.NewMockWorkingDir()
			When(workingDir.GetPullDir(Any[models.Repo](), Any[models.PullRequest]())).ThenReturn(pullDir, nil)
			When(workingDir.GetWorkingDir(Any[models.Repo](), Any[models.PullRequest](), Any[string]())).ThenReturn(repoDir, nil)
			builder := events.NewProjectCommandBuilder(
				false, &config.ParserValidator{}, &events.DefaultProjectFinder{}, nil,
				workingDir, events.NewDefaultWorkingDirLocker(),
				valid.NewGlobalCfgFromArgs(valid.GlobalCfgArgs{}),
				&events.DefaultPendingPlanFinder{}, &events.CommentParser{ExecutableName: "atlantis"},
				false, false, false, false, false, "", defaultUserConfig.AutoplanFileList,
				false, "", false, false, "auto", scope, tfclientmocks.NewMockClient(), &runtime.LocalPlanStore{},
			)
			ctxs, err := builder.BuildApplyCommands(&command.Context{
				Log: logger, Scope: scope, Pull: pull, PullStatus: pullStatus,
			}, &events.CommentCommand{Name: command.Apply})
			if tc.expErr != "" {
				Assert(t, err != nil, "expected validation error")
				Assert(t, strings.Contains(err.Error(), tc.expErr), "got %s", err)
				return
			}
			Ok(t, err)
			Equals(t, tc.failed+tc.pending, len(ctxs))
			for i, ctx := range ctxs {
				Equals(t, fmt.Sprintf("project-%04d", tc.applied+i), ctx.RepoRelDir)
				Equals(t, "default", ctx.Workspace)
				Assert(t, ctx.ExpectedPlanHash != "", "remaining plans must still be hashed")
			}
			// Selecting pending work must not delete artifacts, including plans
			// owned by custom workflows that have already applied successfully.
			for dir := range projects {
				_, err := os.Stat(filepath.Join(repoDir, dir, "default.tfplan"))
				Ok(t, err)
			}
		})
	}
}
