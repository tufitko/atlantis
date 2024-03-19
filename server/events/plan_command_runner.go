package events

import (
	"github.com/pkg/errors"
	"github.com/runatlantis/atlantis/server/core/locking"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/events/vcs"
	"sync"
)

func NewPlanCommandRunner(
	silenceVCSStatusNoPlans bool,
	silenceVCSStatusNoProjects bool,
	vcsClient vcs.Client,
	pendingPlanFinder PendingPlanFinder,
	workingDir WorkingDir,
	commitStatusUpdater CommitStatusUpdater,
	projectCommandBuilder ProjectPlanCommandBuilder,
	projectCommandRunner ProjectPlanCommandRunner,
	dbUpdater *DBUpdater,
	pullUpdater *PullUpdater,
	policyCheckCommandRunner *PolicyCheckCommandRunner,
	autoMerger *AutoMerger,
	parallelPoolSize int,
	SilenceNoProjects bool,
	pullStatusFetcher PullStatusFetcher,
	lockingLocker locking.Locker,
	discardApprovalOnPlan bool,
	pullReqStatusFetcher vcs.PullReqStatusFetcher,
	projectLocker ProjectLocker,
) *PlanCommandRunner {
	return &PlanCommandRunner{
		silenceVCSStatusNoPlans:    silenceVCSStatusNoPlans,
		silenceVCSStatusNoProjects: silenceVCSStatusNoProjects,
		vcsClient:                  vcsClient,
		pendingPlanFinder:          pendingPlanFinder,
		workingDir:                 workingDir,
		commitStatusUpdater:        commitStatusUpdater,
		prjCmdBuilder:              projectCommandBuilder,
		prjCmdRunner:               projectCommandRunner,
		dbUpdater:                  dbUpdater,
		pullUpdater:                pullUpdater,
		policyCheckCommandRunner:   policyCheckCommandRunner,
		autoMerger:                 autoMerger,
		parallelPoolSize:           parallelPoolSize,
		SilenceNoProjects:          SilenceNoProjects,
		pullStatusFetcher:          pullStatusFetcher,
		lockingLocker:              lockingLocker,
		DiscardApprovalOnPlan:      discardApprovalOnPlan,
		pullReqStatusFetcher:       pullReqStatusFetcher,
		projectLocker:              projectLocker,
	}
}

type PlanCommandRunner struct {
	vcsClient vcs.Client
	// SilenceNoProjects is whether Atlantis should respond to PRs if no projects
	// are found
	SilenceNoProjects bool
	// SilenceVCSStatusNoPlans is whether autoplan should set commit status if no plans
	// are found
	silenceVCSStatusNoPlans bool
	// SilenceVCSStatusNoPlans is whether any plan should set commit status if no projects
	// are found
	silenceVCSStatusNoProjects bool
	commitStatusUpdater        CommitStatusUpdater
	pendingPlanFinder          PendingPlanFinder
	workingDir                 WorkingDir
	prjCmdBuilder              ProjectPlanCommandBuilder
	prjCmdRunner               ProjectPlanCommandRunner
	dbUpdater                  *DBUpdater
	pullUpdater                *PullUpdater
	policyCheckCommandRunner   *PolicyCheckCommandRunner
	autoMerger                 *AutoMerger
	parallelPoolSize           int
	pullStatusFetcher          PullStatusFetcher
	lockingLocker              locking.Locker
	projectLocker              ProjectLocker
	mtx                        sync.Mutex
	// DiscardApprovalOnPlan controls if all already existing approvals should be removed/dismissed before executing
	// a plan.
	DiscardApprovalOnPlan bool
	pullReqStatusFetcher  vcs.PullReqStatusFetcher
}

func (p *PlanCommandRunner) runAutoplan(ctx *command.Context) {
	baseRepo := ctx.Pull.BaseRepo
	pull := ctx.Pull

	projectCmds, err := p.prjCmdBuilder.BuildAutoplanCommands(ctx)
	if err != nil {
		if statusErr := p.commitStatusUpdater.UpdateCombined(baseRepo, pull, models.FailedCommitStatus, command.Plan); statusErr != nil {
			ctx.Log.Warn("unable to update commit status: %s", statusErr)
		}
		p.pullUpdater.updatePull(ctx, AutoplanCommand{}, command.Result{Error: err})
		return
	}

	projectCmds, policyCheckCmds := p.partitionProjectCmds(ctx, projectCmds)

	if len(projectCmds) == 0 {
		ctx.Log.Info("determined there was no project to run plan in")
		if !(p.silenceVCSStatusNoPlans || p.silenceVCSStatusNoProjects) {
			// If there were no projects modified, we set successful commit statuses
			// with 0/0 projects planned/policy_checked/applied successfully because some users require
			// the Atlantis status to be passing for all pull requests.
			ctx.Log.Debug("setting VCS status to success with no projects found")
			if err := p.commitStatusUpdater.UpdateCombinedCount(baseRepo, pull, models.SuccessCommitStatus, command.Plan, 0, 0); err != nil {
				ctx.Log.Warn("unable to update commit status: %s", err)
			}
			if err := p.commitStatusUpdater.UpdateCombinedCount(baseRepo, pull, models.SuccessCommitStatus, command.PolicyCheck, 0, 0); err != nil {
				ctx.Log.Warn("unable to update commit status: %s", err)
			}
			if err := p.commitStatusUpdater.UpdateCombinedCount(baseRepo, pull, models.SuccessCommitStatus, command.Apply, 0, 0); err != nil {
				ctx.Log.Warn("unable to update commit status: %s", err)
			}
		}
		return
	}

	// At this point we are sure Atlantis has work to do, so set commit status to pending
	if err := p.commitStatusUpdater.UpdateCombined(ctx.Pull.BaseRepo, ctx.Pull, models.PendingCommitStatus, command.Plan); err != nil {
		ctx.Log.Warn("unable to update plan commit status: %s", err)
	}

	// discard previous plans that might not be relevant anymore
	ctx.Log.Debug("deleting previous plans and locks")
	p.deletePlans(ctx)
	_, err = p.lockingLocker.UnlockByPull(baseRepo.FullName, pull.Num)
	if err != nil {
		ctx.Log.Err("deleting locks: %s", err)
	}

	var projectResults []command.ProjectResult
	if p.projectLocker != nil {
		p.mtx.Lock()
		for _, pctx := range projectCmds {
			lockResult := command.ProjectResult{
				Command:     command.Plan,
				PlanSuccess: nil,
				Error:       nil,
				Failure:     "",
				RepoRelDir:  pctx.RepoRelDir,
				Workspace:   pctx.Workspace,
				ProjectName: pctx.ProjectName,
			}

			// Lock the project
			lockResponse, err := p.projectLocker.TryLock(pctx.Log, pctx.Pull, pctx.User, pctx.Workspace, models.NewProject(pctx.Pull.BaseRepo.FullName, pctx.RepoRelDir, pctx.ProjectName), pctx.RepoLocking)
			if err != nil {
				pctx.Log.Err("locking project: %s", err)
				lockResult.Error = errors.Wrap(err, "acquiring lock")
			} else {
				lockResult.Failure = lockResponse.LockFailureReason
			}
			if lockResult.Error != nil || lockResult.Failure != "" {
				projectResults = append(projectResults, lockResult)
			}
		}
		p.mtx.Unlock()
	}

	var result command.Result

	if len(projectResults) > 0 {
		result = command.Result{
			ProjectResults: projectResults,
		}

		_, err = p.lockingLocker.UnlockByPull(baseRepo.FullName, pull.Num)
		if err != nil {
			ctx.Log.Err("deleting locks: %s", err)
		}
	} else {
		// Only run commands in parallel if enabled
		if p.isParallelEnabled(projectCmds) {
			ctx.Log.Info("Running plans in parallel")
			result = runProjectCmdsParallelGroups(ctx, projectCmds, p.prjCmdRunner.Plan, p.parallelPoolSize)
		} else {
			result = runProjectCmds(projectCmds, p.prjCmdRunner.Plan)
		}
	}

	if p.autoMerger.automergeEnabled(projectCmds) && result.HasErrors() {
		ctx.Log.Info("deleting plans because there were errors and automerge requires all plans succeed")
		p.deletePlans(ctx)
		result.PlansDeleted = true
	}

	p.pullUpdater.updatePull(ctx, AutoplanCommand{}, result)

	pullStatus, err := p.dbUpdater.updateDB(ctx, ctx.Pull, result.ProjectResults)
	if err != nil {
		ctx.Log.Err("writing results: %s", err)
	}

	p.updateCommitStatus(ctx, pullStatus, command.Plan)
	p.updateCommitStatus(ctx, pullStatus, command.Apply)

	// Check if there are any planned projects and if there are any errors or if plans are being deleted
	if len(policyCheckCmds) > 0 &&
		!(result.HasErrors() || result.PlansDeleted) {
		// Run policy_check command
		ctx.Log.Info("Running policy_checks for all plans")

		// refresh ctx's view of pull status since we just wrote to it.
		// realistically each command should refresh this at the start,
		// however, policy checking is weird since it's called within the plan command itself
		// we need to better structure how this command works.
		ctx.PullStatus = &pullStatus

		p.policyCheckCommandRunner.Run(ctx, policyCheckCmds)
	}
}

func (p *PlanCommandRunner) run(ctx *command.Context, cmd *CommentCommand) {
	var err error
	baseRepo := ctx.Pull.BaseRepo
	pull := ctx.Pull

	ctx.PullRequestStatus, err = p.pullReqStatusFetcher.FetchPullStatus(pull)
	if err != nil {
		// On error we continue the request with mergeable assumed false.
		// We want to continue because not all apply's will need this status,
		// only if they rely on the mergeability requirement.
		// All PullRequestStatus fields are set to false by default when error.
		ctx.Log.Warn("unable to get pull request status: %s. Continuing with mergeable and approved assumed false", err)
	}

	if p.DiscardApprovalOnPlan {
		if err = p.pullUpdater.VCSClient.DiscardReviews(baseRepo, pull); err != nil {
			ctx.Log.Err("failed to remove approvals: %s", err)
		}
	}

	if err = p.commitStatusUpdater.UpdateCombined(baseRepo, pull, models.PendingCommitStatus, command.Plan); err != nil {
		ctx.Log.Warn("unable to update commit status: %s", err)
	}

	projectCmds, err := p.prjCmdBuilder.BuildPlanCommands(ctx, cmd)
	if err != nil {
		if statusErr := p.commitStatusUpdater.UpdateCombined(ctx.Pull.BaseRepo, ctx.Pull, models.FailedCommitStatus, command.Plan); statusErr != nil {
			ctx.Log.Warn("unable to update commit status: %s", statusErr)
		}
		p.pullUpdater.updatePull(ctx, cmd, command.Result{Error: err})
		return
	}

	if len(projectCmds) == 0 && p.SilenceNoProjects {
		ctx.Log.Info("determined there was no project to run plan in")
		if !p.silenceVCSStatusNoProjects {
			if cmd.IsForSpecificProject() {
				// With a specific plan, just reset the status so it's not stuck in pending state
				pullStatus, err := p.pullStatusFetcher.GetPullStatus(pull)
				if err != nil {
					ctx.Log.Warn("unable to fetch pull status: %s", err)
					return
				}
				if pullStatus == nil {
					// default to 0/0
					ctx.Log.Debug("setting VCS status to 0/0 success as no previous state was found")
					if err := p.commitStatusUpdater.UpdateCombinedCount(baseRepo, pull, models.SuccessCommitStatus, command.Plan, 0, 0); err != nil {
						ctx.Log.Warn("unable to update commit status: %s", err)
					}
					return
				}
				ctx.Log.Debug("resetting VCS status")
				p.updateCommitStatus(ctx, *pullStatus, command.Plan)
			} else {
				// With a generic plan, we set successful commit statuses
				// with 0/0 projects planned successfully because some users require
				// the Atlantis status to be passing for all pull requests.
				// Does not apply to skipped runs for specific projects
				ctx.Log.Debug("setting VCS status to success with no projects found")
				if err := p.commitStatusUpdater.UpdateCombinedCount(baseRepo, pull, models.SuccessCommitStatus, command.Plan, 0, 0); err != nil {
					ctx.Log.Warn("unable to update commit status: %s", err)
				}
				if err := p.commitStatusUpdater.UpdateCombinedCount(baseRepo, pull, models.SuccessCommitStatus, command.PolicyCheck, 0, 0); err != nil {
					ctx.Log.Warn("unable to update commit status: %s", err)
				}
				if err := p.commitStatusUpdater.UpdateCombinedCount(baseRepo, pull, models.SuccessCommitStatus, command.Apply, 0, 0); err != nil {
					ctx.Log.Warn("unable to update commit status: %s", err)
				}
			}
		}
		return
	}

	projectCmds, policyCheckCmds := p.partitionProjectCmds(ctx, projectCmds)

	// if the plan is generic, new plans will be generated based on changes
	// discard previous plans that might not be relevant anymore
	if !cmd.IsForSpecificProject() {
		ctx.Log.Debug("deleting previous plans and locks")
		p.deletePlans(ctx)
		_, err = p.lockingLocker.UnlockByPull(baseRepo.FullName, pull.Num)
		if err != nil {
			ctx.Log.Err("deleting locks: %s", err)
		}
	}

	var projectResults []command.ProjectResult
	if p.projectLocker != nil {
		p.mtx.Lock()
		for _, pctx := range projectCmds {
			lockResult := command.ProjectResult{
				Command:     command.Plan,
				PlanSuccess: nil,
				Error:       nil,
				Failure:     "",
				RepoRelDir:  pctx.RepoRelDir,
				Workspace:   pctx.Workspace,
				ProjectName: pctx.ProjectName,
			}

			// Lock the project
			lockResponse, err := p.projectLocker.TryLock(pctx.Log, pctx.Pull, pctx.User, pctx.Workspace, models.NewProject(pctx.Pull.BaseRepo.FullName, pctx.RepoRelDir, pctx.ProjectName), pctx.RepoLocking)
			if err != nil {
				pctx.Log.Err("locking project: %s", err)
				lockResult.Error = errors.Wrap(err, "acquiring lock")
			} else {
				lockResult.Failure = lockResponse.LockFailureReason
			}
			if lockResult.Error != nil || lockResult.Failure != "" {
				projectResults = append(projectResults, lockResult)
			}
		}
		p.mtx.Unlock()
	}

	var result command.Result

	if len(projectResults) > 0 {
		result = command.Result{
			ProjectResults: projectResults,
		}

		_, err = p.lockingLocker.UnlockByPull(baseRepo.FullName, pull.Num)
		if err != nil {
			ctx.Log.Err("deleting locks: %s", err)
		}
	} else {
		// Only run commands in parallel if enabled
		if p.isParallelEnabled(projectCmds) {
			ctx.Log.Info("Running plans in parallel")
			result = runProjectCmdsParallelGroups(ctx, projectCmds, p.prjCmdRunner.Plan, p.parallelPoolSize)
		} else {
			result = runProjectCmds(projectCmds, p.prjCmdRunner.Plan)
		}
	}

	if p.autoMerger.automergeEnabled(projectCmds) && result.HasErrors() {
		ctx.Log.Info("deleting plans because there were errors and automerge requires all plans succeed")
		p.deletePlans(ctx)
		result.PlansDeleted = true
	}

	p.pullUpdater.updatePull(
		ctx,
		cmd,
		result)

	pullStatus, err := p.dbUpdater.updateDB(ctx, pull, result.ProjectResults)
	if err != nil {
		ctx.Log.Err("writing results: %s", err)
		return
	}

	p.updateCommitStatus(ctx, pullStatus, command.Plan)
	p.updateCommitStatus(ctx, pullStatus, command.Apply)

	// Runs policy checks step after all plans are successful.
	// This step does not approve any policies that require approval.
	if len(result.ProjectResults) > 0 &&
		!(result.HasErrors() || result.PlansDeleted) {
		ctx.Log.Info("Running policy check for %s", cmd.String())
		p.policyCheckCommandRunner.Run(ctx, policyCheckCmds)
	} else if len(projectCmds) == 0 && !cmd.IsForSpecificProject() {
		// If there were no projects modified, we set successful commit statuses
		// with 0/0 projects planned/policy_checked/applied successfully because some users require
		// the Atlantis status to be passing for all pull requests.
		ctx.Log.Debug("setting VCS status to success with no projects found")
		if err := p.commitStatusUpdater.UpdateCombinedCount(baseRepo, pull, models.SuccessCommitStatus, command.PolicyCheck, 0, 0); err != nil {
			ctx.Log.Warn("unable to update commit status: %s", err)
		}
	}
}

func (p *PlanCommandRunner) Run(ctx *command.Context, cmd *CommentCommand) {
	if ctx.Trigger == command.AutoTrigger {
		p.runAutoplan(ctx)
	} else {
		p.run(ctx, cmd)
	}
}

func (p *PlanCommandRunner) updateCommitStatus(ctx *command.Context, pullStatus models.PullStatus, commandName command.Name) {
	var numSuccess int
	var numErrored int
	status := models.SuccessCommitStatus

	if commandName == command.Plan {
		numErrored = pullStatus.StatusCount(models.ErroredPlanStatus)
		// We consider anything that isn't a plan error as a plan success.
		// For example, if there is an apply error, that means that at least a
		// plan was generated successfully.
		numSuccess = len(pullStatus.Projects) - numErrored

		if numErrored > 0 {
			status = models.FailedCommitStatus
		}
	} else if commandName == command.Apply {
		numSuccess = pullStatus.StatusCount(models.AppliedPlanStatus) + pullStatus.StatusCount(models.PlannedNoChangesPlanStatus)
		numErrored = pullStatus.StatusCount(models.ErroredApplyStatus)

		if numErrored > 0 {
			status = models.FailedCommitStatus
		} else if numSuccess < len(pullStatus.Projects) {
			// If there are plans that haven't been applied yet, no need to update the status
			return
		}
	}

	if err := p.commitStatusUpdater.UpdateCombinedCount(
		ctx.Pull.BaseRepo,
		ctx.Pull,
		status,
		commandName,
		numSuccess,
		len(pullStatus.Projects),
	); err != nil {
		ctx.Log.Warn("unable to update commit status: %s", err)
	}
}

// deletePlans deletes all plans generated in this ctx.
func (p *PlanCommandRunner) deletePlans(ctx *command.Context) {
	pullDir, err := p.workingDir.GetPullDir(ctx.Pull.BaseRepo, ctx.Pull)
	if err != nil {
		ctx.Log.Err("getting pull dir: %s", err)
	}
	if err := p.pendingPlanFinder.DeletePlans(pullDir); err != nil {
		ctx.Log.Err("deleting pending plans: %s", err)
	}
}

func (p *PlanCommandRunner) partitionProjectCmds(
	ctx *command.Context,
	cmds []command.ProjectContext,
) (
	projectCmds []command.ProjectContext,
	policyCheckCmds []command.ProjectContext,
) {
	for _, cmd := range cmds {
		switch cmd.CommandName {
		case command.Plan:
			projectCmds = append(projectCmds, cmd)
		case command.PolicyCheck:
			policyCheckCmds = append(policyCheckCmds, cmd)
		default:
			ctx.Log.Err("%s is not supported", cmd.CommandName)
		}
	}
	return
}

func (p *PlanCommandRunner) isParallelEnabled(projectCmds []command.ProjectContext) bool {
	return len(projectCmds) > 0 && projectCmds[0].ParallelPlanEnabled
}
