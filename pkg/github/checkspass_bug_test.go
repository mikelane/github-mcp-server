package github

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/google/go-github/v82/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_MergePRStack_BugProbe_PendingWithInProgressCheckRun(t *testing.T) {
	// This test probes whether checksPass correctly rejects a PR
	// where combined status is "pending" and a check run is "in_progress".
	// SquashMergeAndCleanup rejects this scenario, but checksPass may not.
	handlers := map[string]http.HandlerFunc{
		GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
			Number: github.Ptr(1),
			State:  github.Ptr("open"),
			Title:  github.Ptr("Test PR"),
			Body:   github.Ptr("body"),
			Head: &github.PullRequestBranch{
				SHA: github.Ptr("abc123"),
				Ref: github.Ptr("feature"),
			},
			Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
		}),
		GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
			State: github.Ptr("pending"),
		}),
		GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
			Total: github.Ptr(1),
			CheckRuns: []*github.CheckRun{
				{
					Name:   github.Ptr("CI Build"),
					Status: github.Ptr("in_progress"),
					// No conclusion set — check is still running
				},
			},
		}),
		// If checksPass incorrectly allows this, the merge endpoint will be called
		PutReposPullsMergeByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequestMergeResult{
			Merged:  github.Ptr(true),
			SHA:     github.Ptr("merged-sha"),
		}),
		PutReposPullsUpdateBranchByOwnerByRepoByPullNumber: mockResponse(t, http.StatusAccepted, nil),
		"DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	}

	client := github.NewClient(MockHTTPClientWithHandlers(handlers))
	serverTool := MergePRStack(translations.NullTranslationHelper)
	deps := BaseDeps{Client: client}
	handler := serverTool.Handler(deps)

	request := createMCPRequest(map[string]any{
		"owner":       "owner",
		"repo":        "repo",
		"pullNumbers": []any{float64(1)},
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)

	textContent := getTextResult(t, result)
	var results []MergePRStackResult
	err = json.Unmarshal([]byte(textContent.Text), &results)
	require.NoError(t, err)

	require.Len(t, results, 1)
	// BUG PROBE: If checksPass correctly rejects in-progress checks,
	// this should be "failed". If it's "merged", checksPass has a bug.
	assert.Equal(t, "failed", results[0].Status,
		"BUG: checksPass allowed merge despite in-progress check run. "+
			"Combined status was 'pending' and check run was 'in_progress' with no conclusion.")
}
