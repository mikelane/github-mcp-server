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

var ToolsetMetadataCompound = inventory.ToolsetMetadata{
	ID:          "compound",
	Description: "Compound actions combining multiple GitHub operations",
	Icon:        "workflow",
}

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
			if v, vErr := OptionalParam[bool](args, "deleteRemoteBranch"); vErr != nil {
				return utils.NewToolResultError(vErr.Error()), nil, nil
			} else if v == false {
				// Check if the parameter was explicitly provided as false
				if _, ok := args["deleteRemoteBranch"]; ok {
					deleteRemoteBranch = false
				}
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

			// Step 4: If any checks are failing, return error with details
			if combinedStatus.GetState() == "failure" || combinedStatus.GetState() == "error" {
				var failingChecks []string
				for _, cr := range checkRuns.CheckRuns {
					conclusion := cr.GetConclusion()
					if conclusion == "failure" || conclusion == "error" || conclusion == "cancelled" {
						failingChecks = append(failingChecks, cr.GetName())
					}
				}
				return utils.NewToolResultError(fmt.Sprintf(
					"cannot merge: checks are failing. Failed checks: %s",
					strings.Join(failingChecks, ", "),
				)), nil, nil
			}

			// Also check individual check runs for failures even if combined status is not "failure"
			var failingCheckRuns []string
			for _, cr := range checkRuns.CheckRuns {
				conclusion := cr.GetConclusion()
				if conclusion == "failure" || conclusion == "error" {
					failingCheckRuns = append(failingCheckRuns, cr.GetName())
				}
			}
			if len(failingCheckRuns) > 0 {
				return utils.NewToolResultError(fmt.Sprintf(
					"cannot merge: checks are failing. Failed checks: %s",
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
				if err != nil {
					// 422 means the branch was already deleted — treat as success
					if resp != nil && resp.StatusCode == http.StatusUnprocessableEntity {
						branchDeleted = false
					} else {
						return ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to delete branch", resp, err), nil, nil
					}
				} else {
					branchDeleted = true
					defer func() { _ = resp.Body.Close() }()
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
