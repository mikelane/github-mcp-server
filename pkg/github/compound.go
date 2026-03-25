package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	ghErrors "github.com/github/github-mcp-server/pkg/errors"
	"github.com/github/github-mcp-server/pkg/inventory"
	"github.com/github/github-mcp-server/pkg/scopes"
	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/github/github-mcp-server/pkg/utils"
	"github.com/google/go-github/v82/github"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type squashMergeAndCleanupResult struct {
	MergedSHA      string `json:"mergedSHA"`
	BranchName     string `json:"branchName"`
	BaseBranch     string `json:"baseBranch"`
	BranchDeleted  bool   `json:"branchDeleted"`
}

// SquashMergeAndCleanup creates a tool that checks CI status, squash merges a PR,
// and optionally deletes the remote branch in a single operation.
func SquashMergeAndCleanup(t translations.TranslationHelperFunc) inventory.ServerTool {
	schema := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"owner": {
				Type:        "string",
				Description: "Repository owner",
			},
			"repo": {
				Type:        "string",
				Description: "Repository name",
			},
			"pullNumber": {
				Type:        "number",
				Description: "Pull request number",
			},
			"commitTitle": {
				Type:        "string",
				Description: "Custom title for the squash merge commit. Defaults to the PR title.",
			},
			"commitMessage": {
				Type:        "string",
				Description: "Custom message for the squash merge commit. Defaults to the PR body.",
			},
			"deleteRemoteBranch": {
				Type:        "boolean",
				Description: "Whether to delete the remote branch after merging. Defaults to true.",
			},
		},
		Required: []string{"owner", "repo", "pullNumber"},
	}

	return NewTool(
		ToolsetMetadataCompound,
		mcp.Tool{
			Name:        "squash_merge_and_cleanup",
			Description: t("TOOL_SQUASH_MERGE_AND_CLEANUP_DESCRIPTION", "Check CI status, squash merge a pull request, and delete the remote branch in one operation."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_SQUASH_MERGE_AND_CLEANUP_USER_TITLE", "Squash merge and cleanup"),
				ReadOnlyHint: false,
			},
			InputSchema: schema,
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			pullNumber, err := RequiredInt(args, "pullNumber")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			commitTitle, err := OptionalParam[string](args, "commitTitle")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			commitMessage, err := OptionalParam[string](args, "commitMessage")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			deleteRemoteBranch := true
			if v, ok, err := OptionalParamOK[bool](args, "deleteRemoteBranch"); err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			} else if ok {
				deleteRemoteBranch = v
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return utils.NewToolResultErrorFromErr("failed to get GitHub client", err), nil, nil
			}

			// Step 1: Get the PR and verify it is open
			pr, resp, err := client.PullRequests.Get(ctx, owner, repo, pullNumber)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to get pull request", resp, err), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if pr.GetState() != "open" {
				return utils.NewToolResultError(fmt.Sprintf("pull request #%d is not open (state: %s)", pullNumber, pr.GetState())), nil, nil
			}

			headSHA := pr.GetHead().GetSHA()
			branchName := pr.GetHead().GetRef()
			baseBranch := pr.GetBase().GetRef()

			// Step 2: Get combined commit status
			combinedStatus, resp, err := client.Repositories.GetCombinedStatus(ctx, owner, repo, headSHA, nil)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to get combined status", resp, err), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			// Step 3: Get check runs
			checkRuns, resp, err := client.Checks.ListCheckRunsForRef(ctx, owner, repo, headSHA, nil)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to get check runs", resp, err), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			// Step 4: Verify all checks pass using an allow-list approach.
			// Combined status must be "success", or "pending" only when no CI is configured.
			combinedState := combinedStatus.GetState()
			if combinedState != "success" {
				if combinedState != "pending" || len(combinedStatus.Statuses) != 0 || len(checkRuns.CheckRuns) != 0 {
					var failingChecks []string
					for _, s := range combinedStatus.Statuses {
						if s.GetState() != "success" {
							failingChecks = append(failingChecks, fmt.Sprintf("%s (%s)", s.GetContext(), s.GetState()))
						}
					}
					for _, cr := range checkRuns.CheckRuns {
						if cr.GetStatus() != "completed" || (cr.GetConclusion() != "success" && cr.GetConclusion() != "neutral" && cr.GetConclusion() != "skipped") {
							status := cr.GetStatus()
							if status == "completed" {
								status = cr.GetConclusion()
							}
							failingChecks = append(failingChecks, fmt.Sprintf("%s (%s)", cr.GetName(), status))
						}
					}
					return utils.NewToolResultError(fmt.Sprintf(
						"cannot merge: checks are not passing (combined status: %s). Failing checks: %s",
						combinedState, strings.Join(failingChecks, ", "),
					)), nil, nil
				}
			}

			// Verify each individual check run has completed successfully
			var failingCheckRuns []string
			for _, cr := range checkRuns.CheckRuns {
				if cr.GetStatus() != "completed" {
					failingCheckRuns = append(failingCheckRuns, fmt.Sprintf("%s (status: %s)", cr.GetName(), cr.GetStatus()))
				} else {
					conclusion := cr.GetConclusion()
					if conclusion != "success" && conclusion != "neutral" && conclusion != "skipped" {
						failingCheckRuns = append(failingCheckRuns, fmt.Sprintf("%s (conclusion: %s)", cr.GetName(), conclusion))
					}
				}
			}
			if len(failingCheckRuns) > 0 {
				return utils.NewToolResultError(fmt.Sprintf(
					"cannot merge: check runs are not passing. Failing checks: %s",
					strings.Join(failingCheckRuns, ", "),
				)), nil, nil
			}

			// Step 5: Build merge options
			mergeTitle := commitTitle
			if mergeTitle == "" {
				mergeTitle = pr.GetTitle()
			}
			mergeMessage := commitMessage
			if mergeMessage == "" {
				mergeMessage = pr.GetBody()
			}

			options := &github.PullRequestOptions{
				CommitTitle: mergeTitle,
				MergeMethod: "squash",
			}

			// Step 6: Merge the PR
			mergeResult, resp, err := client.PullRequests.Merge(ctx, owner, repo, pullNumber, mergeMessage, options)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to merge pull request", resp, err), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			// Step 7: Optionally delete the remote branch
			branchDeleted := false
			if deleteRemoteBranch {
				ref := fmt.Sprintf("heads/%s", branchName)
				resp, err := client.Git.DeleteRef(ctx, owner, repo, ref)
				if resp != nil && resp.Body != nil {
					defer func() { _ = resp.Body.Close() }()
				}
				if err != nil {
					// 422 means the branch was already deleted — treat as success
					if resp != nil && resp.StatusCode == http.StatusUnprocessableEntity {
						branchDeleted = false
					} else {
						return ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to delete branch", resp, err), nil, nil
					}
				} else {
					branchDeleted = true
				}
			}

			// Step 8: Return result
			result := squashMergeAndCleanupResult{
				MergedSHA:     mergeResult.GetSHA(),
				BranchName:    branchName,
				BaseBranch:    baseBranch,
				BranchDeleted: branchDeleted,
			}

			r, err := json.Marshal(result)
			if err != nil {
				return utils.NewToolResultErrorFromErr("failed to marshal response", err), nil, nil
			}

			return utils.NewToolResultText(string(r)), nil, nil
		},
	)
}

// MergePRStackResult represents the outcome of merging a single PR in the stack.
type MergePRStackResult struct {
	PullNumber int    `json:"pullNumber"`
	Status     string `json:"status"`
	MergedSHA  string `json:"mergedSHA,omitempty"`
	Error      string `json:"error,omitempty"`
}

// MergePRStack creates a tool that merges an ordered list of stacked PRs,
// updating base branches between each merge.
func MergePRStack(t translations.TranslationHelperFunc) inventory.ServerTool {
	schema := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"owner": {
				Type:        "string",
				Description: "Repository owner",
			},
			"repo": {
				Type:        "string",
				Description: "Repository name",
			},
			"pullNumbers": {
				Type:        "array",
				Description: "PR numbers in merge order (base PR first)",
				Items: &jsonschema.Schema{
					Type: "number",
				},
			},
			"deleteRemoteBranches": {
				Type:        "boolean",
				Description: "Delete remote branches after merging (default: true)",
			},
		},
		Required: []string{"owner", "repo", "pullNumbers"},
	}

	return NewTool(
		ToolsetMetadataCompound,
		mcp.Tool{
			Name:        "merge_pr_stack",
			Description: t("TOOL_MERGE_PR_STACK_DESCRIPTION", "Merge an ordered list of stacked pull requests, updating base branches between each merge."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_MERGE_PR_STACK_USER_TITLE", "Merge PR stack"),
				ReadOnlyHint: false,
			},
			InputSchema: schema,
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			pullNumbersRaw, ok := args["pullNumbers"]
			if !ok {
				return utils.NewToolResultError("missing required parameter: pullNumbers"), nil, nil
			}
			pullNumbersSlice, ok := pullNumbersRaw.([]any)
			if !ok {
				return utils.NewToolResultError("pullNumbers must be an array"), nil, nil
			}

			pullNumbers := make([]int, 0, len(pullNumbersSlice))
			for _, v := range pullNumbersSlice {
				n, ok := v.(float64)
				if !ok {
					return utils.NewToolResultError(fmt.Sprintf("pullNumbers must contain numbers, got %T", v)), nil, nil
				}
				pullNumbers = append(pullNumbers, int(n))
			}

			deleteRemoteBranches := true
			if val, ok, err := OptionalParamOK[bool](args, "deleteRemoteBranches"); err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			} else if ok {
				deleteRemoteBranches = val
			}

			if len(pullNumbers) == 0 {
				r, err := json.Marshal([]MergePRStackResult{})
				if err != nil {
					return utils.NewToolResultError("failed to marshal empty result"), nil, nil
				}
				return utils.NewToolResultText(string(r)), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("failed to get GitHub client: %v", err)), nil, nil
			}

			results := mergePRStack(ctx, client, owner, repo, pullNumbers, deleteRemoteBranches)

			r, err := json.Marshal(results)
			if err != nil {
				return utils.NewToolResultError("failed to marshal results"), nil, nil
			}
			return utils.NewToolResultText(string(r)), nil, nil
		},
	)
}

func mergePRStack(ctx context.Context, client *github.Client, owner, repo string, pullNumbers []int, deleteRemoteBranches bool) []MergePRStackResult {
	results := make([]MergePRStackResult, len(pullNumbers))

	for i, prNum := range pullNumbers {
		results[i].PullNumber = prNum

		pr, resp, err := client.PullRequests.Get(ctx, owner, repo, prNum)
		if err != nil {
			results[i].Status = "failed"
			results[i].Error = fmt.Sprintf("failed to get PR #%d: %v", prNum, err)
			markRemainingSkipped(results, i+1)
			return results
		}
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
		}

		if pr.GetState() != "open" {
			results[i].Status = "failed"
			results[i].Error = fmt.Sprintf("PR #%d is not open (state: %s)", prNum, pr.GetState())
			markRemainingSkipped(results, i+1)
			return results
		}

		headSHA := pr.GetHead().GetSHA()

		if !checksPass(ctx, client, owner, repo, headSHA) {
			results[i].Status = "failed"
			results[i].Error = fmt.Sprintf("PR #%d has failing checks", prNum)
			markRemainingSkipped(results, i+1)
			return results
		}

		mergeResult, mergeResp, err := client.PullRequests.Merge(ctx, owner, repo, prNum, "", &github.PullRequestOptions{
			CommitTitle: pr.GetTitle(),
			MergeMethod: "squash",
		})
		if err != nil || mergeResp.StatusCode != http.StatusOK {
			results[i].Status = "failed"
			errMsg := fmt.Sprintf("failed to merge PR #%d", prNum)
			if err != nil {
				errMsg = fmt.Sprintf("%s: %v", errMsg, err)
			}
			results[i].Error = errMsg
			markRemainingSkipped(results, i+1)
			return results
		}
		defer func() { _ = mergeResp.Body.Close() }()

		results[i].Status = "merged"
		results[i].MergedSHA = mergeResult.GetSHA()

		if deleteRemoteBranches {
			branchRef := "heads/" + pr.GetHead().GetRef()
			// Branch deletion failure is non-fatal; continue with the stack
			_, _ = client.Git.DeleteRef(ctx, owner, repo, branchRef)
		}

		// If there are more PRs in the stack, update the next PR's branch
		if i < len(pullNumbers)-1 {
			nextPRNum := pullNumbers[i+1]
			// Branch update errors (including HTTP 202 accepted) are non-fatal
			_, _, _ = client.PullRequests.UpdateBranch(ctx, owner, repo, nextPRNum, nil)
		}
	}

	return results
}

func checksPass(ctx context.Context, client *github.Client, owner, repo, ref string) bool {
	combinedStatus, _, err := client.Repositories.GetCombinedStatus(ctx, owner, repo, ref, nil)
	if err != nil {
		return false
	}

	checkRuns, _, err := client.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, nil)
	if err != nil {
		return false
	}

	// Allow-list approach: combined status must be "success", or "pending" only
	// when no CI is configured (zero statuses and zero check runs).
	combinedState := combinedStatus.GetState()
	if combinedState != "success" {
		if combinedState != "pending" || len(combinedStatus.Statuses) != 0 || len(checkRuns.CheckRuns) != 0 {
			return false
		}
	}

	// Each check run must be completed with an acceptable conclusion.
	for _, cr := range checkRuns.CheckRuns {
		if cr.GetStatus() != "completed" {
			return false
		}
		conclusion := cr.GetConclusion()
		if conclusion != "success" && conclusion != "neutral" && conclusion != "skipped" {
			return false
		}
	}

	return true
}

func markRemainingSkipped(results []MergePRStackResult, startIdx int) {
	for j := startIdx; j < len(results); j++ {
		results[j].Status = "skipped"
	}
}
