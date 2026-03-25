package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
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

// BatchPRStatusResult holds the structured status for a single pull request.
type BatchPRStatusResult struct {
	Number       int      `json:"number"`
	Title        string   `json:"title"`
	Author       string   `json:"author"`
	Branch       string   `json:"branch"`
	BaseBranch   string   `json:"baseBranch"`
	CI           string   `json:"ci"`
	ReviewStatus string   `json:"reviewStatus"`
	Reviewers    []string `json:"reviewers"`
	LinkedIssues []int    `json:"linkedIssues"`
	Mergeable    bool     `json:"mergeable"`
	Draft        bool     `json:"draft"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt"`
	Additions    int      `json:"additions"`
	Deletions    int      `json:"deletions"`
	ChangedFiles int      `json:"changedFiles"`
}

var linkedIssuePattern = regexp.MustCompile(`(?i)(?:closes|fixes|resolves)\s+#(\d+)`)

func BatchPRStatus(t translations.TranslationHelperFunc) inventory.ServerTool {
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
			"state": {
				Type:        "string",
				Description: "Filter pull requests by state (default: open)",
				Enum:        []any{"open", "closed", "all"},
			},
			"labels": {
				Type:        "array",
				Description: "Filter pull requests by labels",
				Items: &jsonschema.Schema{
					Type: "string",
				},
			},
			"head": {
				Type:        "string",
				Description: "Filter by head branch (user:ref-name or org:ref-name)",
			},
		},
		Required: []string{"owner", "repo"},
	}

	return NewTool(
		ToolsetMetadataCompound,
		mcp.Tool{
			Name:        "batch_pr_status",
			Description: t("TOOL_BATCH_PR_STATUS_DESCRIPTION", "Get a structured overview of all pull requests with CI status, review status, linked issues, and age."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_BATCH_PR_STATUS_USER_TITLE", "Batch pull request status overview"),
				ReadOnlyHint: true,
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
			state, err := OptionalParam[string](args, "state")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			if state == "" {
				state = "open"
			}
			labels, err := OptionalStringArrayParam(args, "labels")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			head, err := OptionalParam[string](args, "head")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return utils.NewToolResultErrorFromErr("failed to get GitHub client", err), nil, nil
			}

			opts := &github.PullRequestListOptions{
				State: state,
				Head:  head,
				ListOptions: github.ListOptions{
					PerPage: 30,
				},
			}

			prs, resp, err := client.PullRequests.List(ctx, owner, repo, opts)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to list pull requests", resp, err), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			// Filter by labels client-side since the List PRs API does not support label filtering
			if len(labels) > 0 {
				prs = filterPRsByLabels(prs, labels)
			}

			results := make([]BatchPRStatusResult, 0, len(prs))
			for _, pr := range prs {
				result, err := buildPRStatusResult(ctx, client, owner, repo, pr)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to build status for PR #%d: %w", pr.GetNumber(), err)
				}
				results = append(results, result)
			}

			return MarshalledTextResult(results), nil, nil
		},
	)
}

func buildPRStatusResult(ctx context.Context, client *github.Client, owner, repo string, pr *github.PullRequest) (BatchPRStatusResult, error) {
	headSHA := pr.GetHead().GetSHA()

	// Get combined commit status
	status, _, err := client.Repositories.GetCombinedStatus(ctx, owner, repo, headSHA, nil)
	if err != nil {
		return BatchPRStatusResult{}, fmt.Errorf("failed to get combined status: %w", err)
	}
	ciState := status.GetState()

	// Get reviews
	reviews, _, err := client.PullRequests.ListReviews(ctx, owner, repo, pr.GetNumber(), nil)
	if err != nil {
		return BatchPRStatusResult{}, fmt.Errorf("failed to get reviews: %w", err)
	}
	reviewStatus, reviewers := summarizeReviews(reviews)

	// Parse linked issues from body
	linkedIssues := parseLinkedIssues(pr.GetBody())

	return BatchPRStatusResult{
		Number:       pr.GetNumber(),
		Title:        pr.GetTitle(),
		Author:       pr.GetUser().GetLogin(),
		Branch:       pr.GetHead().GetRef(),
		BaseBranch:   pr.GetBase().GetRef(),
		CI:           ciState,
		ReviewStatus: reviewStatus,
		Reviewers:    reviewers,
		LinkedIssues: linkedIssues,
		Mergeable:    pr.GetMergeable(),
		Draft:        pr.GetDraft(),
		CreatedAt:    pr.GetCreatedAt().UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    pr.GetUpdatedAt().UTC().Format("2006-01-02T15:04:05Z"),
		Additions:    pr.GetAdditions(),
		Deletions:    pr.GetDeletions(),
		ChangedFiles: pr.GetChangedFiles(),
	}, nil
}

func summarizeReviews(reviews []*github.PullRequestReview) (string, []string) {
	if len(reviews) == 0 {
		return "none", []string{}
	}

	// Track the latest review state per reviewer
	latestByUser := make(map[string]string)
	for _, review := range reviews {
		user := review.GetUser().GetLogin()
		state := strings.ToLower(review.GetState())
		if user != "" && state != "" {
			latestByUser[user] = state
		}
	}

	if len(latestByUser) == 0 {
		return "none", []string{}
	}

	// Determine overall status: changes_requested > approved > pending
	hasApproved := false
	hasChangesRequested := false
	reviewers := make([]string, 0, len(latestByUser))
	for user, state := range latestByUser {
		reviewers = append(reviewers, user)
		switch state {
		case "approved":
			hasApproved = true
		case "changes_requested":
			hasChangesRequested = true
		}
	}

	var status string
	switch {
	case hasChangesRequested:
		status = "changes_requested"
	case hasApproved:
		status = "approved"
	default:
		status = "pending"
	}

	return status, reviewers
}

func parseLinkedIssues(body string) []int {
	matches := linkedIssuePattern.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return []int{}
	}

	issues := make([]int, 0, len(matches))
	for _, match := range matches {
		if len(match) >= 2 {
			num, err := strconv.Atoi(match[1])
			if err == nil {
				issues = append(issues, num)
			}
		}
	}
	return issues
}

func filterPRsByLabels(prs []*github.PullRequest, labels []string) []*github.PullRequest {
	labelSet := make(map[string]bool, len(labels))
	for _, l := range labels {
		labelSet[strings.ToLower(l)] = true
	}

	filtered := make([]*github.PullRequest, 0)
	for _, pr := range prs {
		if prHasAnyLabel(pr, labelSet) {
			filtered = append(filtered, pr)
		}
	}
	return filtered
}

func prHasAnyLabel(pr *github.PullRequest, labelSet map[string]bool) bool {
	for _, label := range pr.Labels {
		if labelSet[strings.ToLower(label.GetName())] {
			return true
		}
	}
	return false
}
