package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/github/github-mcp-server/internal/githubv4mock"
	"github.com/github/github-mcp-server/internal/toolsnaps"
	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/google/go-github/v82/github"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_SquashMergeAndCleanup(t *testing.T) {
	// Verify tool definition once
	serverTool := SquashMergeAndCleanup(translations.NullTranslationHelper)
	tool := serverTool.Tool
	require.NoError(t, toolsnaps.Test(tool.Name, tool))

	assert.Equal(t, "squash_merge_and_cleanup", tool.Name)
	assert.NotEmpty(t, tool.Description)
	schema := tool.InputSchema.(*jsonschema.Schema)
	assert.Contains(t, schema.Properties, "owner")
	assert.Contains(t, schema.Properties, "repo")
	assert.Contains(t, schema.Properties, "pullNumber")
	assert.Contains(t, schema.Properties, "commitTitle")
	assert.Contains(t, schema.Properties, "commitMessage")
	assert.Contains(t, schema.Properties, "deleteRemoteBranch")
	assert.ElementsMatch(t, schema.Required, []string{"owner", "repo", "pullNumber"})

	t.Run("merges open PR with passing checks and deletes branch", func(t *testing.T) {
		headSHA := "abc123def456"
		mergedSHA := "merged789xyz"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			// GET PR
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
				Title: github.Ptr("Add feature"),
				Body:  github.Ptr("Feature description"),
			}),
			// GET combined status
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			// GET check runs
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(1),
				CheckRuns: []*github.CheckRun{
					{
						Name:       github.Ptr("CI"),
						Status:     github.Ptr("completed"),
						Conclusion: github.Ptr("success"),
					},
				},
			}),
			// PUT merge
			PutReposPullsMergeByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequestMergeResult{
				Merged:  github.Ptr(true),
				Message: github.Ptr("Pull Request successfully merged"),
				SHA:     github.Ptr(mergedSHA),
			}),
			// DELETE branch ref
			"DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}": mockResponse(t, http.StatusNoContent, nil),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var response map[string]any
		err = json.Unmarshal([]byte(textContent.Text), &response)
		require.NoError(t, err)

		assert.Equal(t, mergedSHA, response["mergedSHA"])
		assert.Equal(t, "feature-branch", response["branchName"])
		assert.Equal(t, "main", response["baseBranch"])
		assert.Equal(t, true, response["branchDeleted"])
	})

	t.Run("returns error when PR is not open", func(t *testing.T) {
		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("closed"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr("abc123"),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "not open")
	})

	t.Run("returns error when checks are failing", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("failure"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(2),
				CheckRuns: []*github.CheckRun{
					{
						Name:       github.Ptr("CI"),
						Status:     github.Ptr("completed"),
						Conclusion: github.Ptr("success"),
					},
					{
						Name:       github.Ptr("Lint"),
						Status:     github.Ptr("completed"),
						Conclusion: github.Ptr("failure"),
					},
				},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "Lint")
		assert.Contains(t, errorContent.Text, "Failing")
	})

	t.Run("rejects merge when combined status is pending with actual statuses", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("pending"),
				Statuses: []*github.RepoStatus{
					{
						Context: github.Ptr("ci/build"),
						State:   github.Ptr("pending"),
					},
				},
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total:     github.Ptr(0),
				CheckRuns: []*github.CheckRun{},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "cannot merge")
		assert.Contains(t, errorContent.Text, "ci/build")
	})

	t.Run("rejects merge when check runs are in progress", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("pending"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(1),
				CheckRuns: []*github.CheckRun{
					{
						Name:   github.Ptr("CI"),
						Status: github.Ptr("in_progress"),
					},
				},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "cannot merge")
		assert.Contains(t, errorContent.Text, "CI")
	})

	t.Run("merges when no CI is configured (pending with zero statuses and zero check runs)", func(t *testing.T) {
		headSHA := "abc123def456"
		mergedSHA := "merged789xyz"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
				Title: github.Ptr("Add feature"),
				Body:  github.Ptr("Feature description"),
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State:    github.Ptr("pending"),
				Statuses: []*github.RepoStatus{},
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total:     github.Ptr(0),
				CheckRuns: []*github.CheckRun{},
			}),
			PutReposPullsMergeByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequestMergeResult{
				Merged:  github.Ptr(true),
				Message: github.Ptr("Pull Request successfully merged"),
				SHA:     github.Ptr(mergedSHA),
			}),
			"DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}": mockResponse(t, http.StatusNoContent, nil),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var response map[string]any
		err = json.Unmarshal([]byte(textContent.Text), &response)
		require.NoError(t, err)
		assert.Equal(t, mergedSHA, response["mergedSHA"])
	})

	t.Run("returns error on merge conflict (409) without attempting branch deletion", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
				Title: github.Ptr("Add feature"),
				Body:  github.Ptr("Feature description"),
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total:     github.Ptr(0),
				CheckRuns: []*github.CheckRun{},
			}),
			PutReposPullsMergeByOwnerByRepoByPullNumber: mockResponse(t, http.StatusConflict, map[string]string{
				"message": "Head branch was modified. Review and try the merge again.",
			}),
			// No DELETE handler — if branch deletion is attempted, it will 404 and fail the test
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "failed to merge")
	})

	t.Run("uses custom commit title and message", func(t *testing.T) {
		headSHA := "abc123def456"
		mergedSHA := "merged789xyz"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
				Title: github.Ptr("Add feature"),
				Body:  github.Ptr("Feature description"),
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total:     github.Ptr(0),
				CheckRuns: []*github.CheckRun{},
			}),
			PutReposPullsMergeByOwnerByRepoByPullNumber: expectRequestBody(t, map[string]any{
				"commit_title":   "Custom title",
				"commit_message": "Custom message",
				"merge_method":   "squash",
			}).andThen(
				mockResponse(t, http.StatusOK, &github.PullRequestMergeResult{
					Merged:  github.Ptr(true),
					Message: github.Ptr("Pull Request successfully merged"),
					SHA:     github.Ptr(mergedSHA),
				}),
			),
			"DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}": mockResponse(t, http.StatusNoContent, nil),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":         "owner",
			"repo":          "repo",
			"pullNumber":    float64(42),
			"commitTitle":   "Custom title",
			"commitMessage": "Custom message",
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var response map[string]any
		err = json.Unmarshal([]byte(textContent.Text), &response)
		require.NoError(t, err)
		assert.Equal(t, mergedSHA, response["mergedSHA"])
	})

	t.Run("skips branch deletion when deleteRemoteBranch is false", func(t *testing.T) {
		headSHA := "abc123def456"
		mergedSHA := "merged789xyz"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
				Title: github.Ptr("Add feature"),
				Body:  github.Ptr("Feature description"),
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total:     github.Ptr(0),
				CheckRuns: []*github.CheckRun{},
			}),
			PutReposPullsMergeByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequestMergeResult{
				Merged:  github.Ptr(true),
				Message: github.Ptr("Pull Request successfully merged"),
				SHA:     github.Ptr(mergedSHA),
			}),
			// No DELETE handler registered — if branch deletion is attempted, it will 404
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":              "owner",
			"repo":               "repo",
			"pullNumber":         float64(42),
			"deleteRemoteBranch": false,
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var response map[string]any
		err = json.Unmarshal([]byte(textContent.Text), &response)
		require.NoError(t, err)
		assert.Equal(t, mergedSHA, response["mergedSHA"])
		assert.Equal(t, false, response["branchDeleted"])
	})

	t.Run("handles branch already deleted (422) gracefully", func(t *testing.T) {
		headSHA := "abc123def456"
		mergedSHA := "merged789xyz"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
				Title: github.Ptr("Add feature"),
				Body:  github.Ptr("Feature description"),
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total:     github.Ptr(0),
				CheckRuns: []*github.CheckRun{},
			}),
			PutReposPullsMergeByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequestMergeResult{
				Merged:  github.Ptr(true),
				Message: github.Ptr("Pull Request successfully merged"),
				SHA:     github.Ptr(mergedSHA),
			}),
			// DELETE returns 422 — branch already deleted
			"DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}": mockResponse(t, http.StatusUnprocessableEntity, map[string]string{
				"message": "Reference does not exist",
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var response map[string]any
		err = json.Unmarshal([]byte(textContent.Text), &response)
		require.NoError(t, err)
		assert.Equal(t, mergedSHA, response["mergedSHA"])
		assert.Equal(t, false, response["branchDeleted"])
	})

	// === Parameter validation error tests ===

	t.Run("returns error when owner param is missing", func(t *testing.T) {
		deps := stubDeps{}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "owner")
	})

	t.Run("returns error when repo param is missing", func(t *testing.T) {
		deps := stubDeps{}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "repo")
	})

	t.Run("returns error when pullNumber param is missing", func(t *testing.T) {
		deps := stubDeps{}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner": "owner",
			"repo":  "repo",
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "pullNumber")
	})

	t.Run("returns error when commitTitle has wrong type", func(t *testing.T) {
		deps := stubDeps{}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumber":  float64(42),
			"commitTitle": 12345, // wrong type: number instead of string
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "commitTitle")
	})

	t.Run("returns error when commitMessage has wrong type", func(t *testing.T) {
		deps := stubDeps{}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":         "owner",
			"repo":          "repo",
			"pullNumber":    float64(42),
			"commitMessage": true, // wrong type: bool instead of string
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "commitMessage")
	})

	t.Run("returns error when deleteRemoteBranch has wrong type", func(t *testing.T) {
		deps := stubDeps{}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":              "owner",
			"repo":               "repo",
			"pullNumber":         float64(42),
			"deleteRemoteBranch": "not-a-bool",
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "deleteRemoteBranch")
	})

	// === GetClient error ===

	t.Run("returns error when GetClient fails", func(t *testing.T) {
		deps := stubDeps{clientFn: stubClientFnErr("auth token expired")}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "failed to get GitHub client")
		assert.Contains(t, errorContent.Text, "auth token expired")
	})

	// === API call failure tests ===

	t.Run("returns error when PR Get API call fails", func(t *testing.T) {
		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusInternalServerError, map[string]string{
				"message": "internal server error",
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "failed to get pull request")
	})

	t.Run("returns error when GetCombinedStatus API call fails", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusInternalServerError, map[string]string{
				"message": "status check unavailable",
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "failed to get combined status")
	})

	t.Run("returns error when ListCheckRunsForRef API call fails", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusInternalServerError, map[string]string{
				"message": "check runs unavailable",
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "failed to get check runs")
	})

	// === CI gate triangulation: check run conclusions ===

	t.Run("allows merge when check run conclusion is neutral", func(t *testing.T) {
		headSHA := "abc123def456"
		mergedSHA := "merged789xyz"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
				Title: github.Ptr("Add feature"),
				Body:  github.Ptr("Feature description"),
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(1),
				CheckRuns: []*github.CheckRun{
					{
						Name:       github.Ptr("Optional Check"),
						Status:     github.Ptr("completed"),
						Conclusion: github.Ptr("neutral"),
					},
				},
			}),
			PutReposPullsMergeByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequestMergeResult{
				Merged:  github.Ptr(true),
				Message: github.Ptr("Pull Request successfully merged"),
				SHA:     github.Ptr(mergedSHA),
			}),
			"DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}": mockResponse(t, http.StatusNoContent, nil),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var response map[string]any
		err = json.Unmarshal([]byte(textContent.Text), &response)
		require.NoError(t, err)
		assert.Equal(t, mergedSHA, response["mergedSHA"])
	})

	t.Run("allows merge when check run conclusion is skipped", func(t *testing.T) {
		headSHA := "abc123def456"
		mergedSHA := "merged789xyz"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
				Title: github.Ptr("Add feature"),
				Body:  github.Ptr("Feature description"),
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(1),
				CheckRuns: []*github.CheckRun{
					{
						Name:       github.Ptr("Conditional Build"),
						Status:     github.Ptr("completed"),
						Conclusion: github.Ptr("skipped"),
					},
				},
			}),
			PutReposPullsMergeByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequestMergeResult{
				Merged:  github.Ptr(true),
				Message: github.Ptr("Pull Request successfully merged"),
				SHA:     github.Ptr(mergedSHA),
			}),
			"DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}": mockResponse(t, http.StatusNoContent, nil),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var response map[string]any
		err = json.Unmarshal([]byte(textContent.Text), &response)
		require.NoError(t, err)
		assert.Equal(t, mergedSHA, response["mergedSHA"])
	})

	t.Run("rejects merge when check run conclusion is cancelled", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(1),
				CheckRuns: []*github.CheckRun{
					{
						Name:       github.Ptr("CI"),
						Status:     github.Ptr("completed"),
						Conclusion: github.Ptr("cancelled"),
					},
				},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "CI")
		assert.Contains(t, errorContent.Text, "cancelled")
	})

	t.Run("rejects merge when check run conclusion is timed_out", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(1),
				CheckRuns: []*github.CheckRun{
					{
						Name:       github.Ptr("Long Build"),
						Status:     github.Ptr("completed"),
						Conclusion: github.Ptr("timed_out"),
					},
				},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "Long Build")
		assert.Contains(t, errorContent.Text, "timed_out")
	})

	t.Run("rejects merge when check run conclusion is action_required", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(1),
				CheckRuns: []*github.CheckRun{
					{
						Name:       github.Ptr("Security Scan"),
						Status:     github.Ptr("completed"),
						Conclusion: github.Ptr("action_required"),
					},
				},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "Security Scan")
		assert.Contains(t, errorContent.Text, "action_required")
	})

	// === CI gate: check runs in the first rejection block (combined status != success) ===

	t.Run("reports check run with cancelled conclusion in combined failure block", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State:    github.Ptr("failure"),
				Statuses: []*github.RepoStatus{},
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(2),
				CheckRuns: []*github.CheckRun{
					{
						Name:       github.Ptr("Build"),
						Status:     github.Ptr("completed"),
						Conclusion: github.Ptr("success"),
					},
					{
						Name:       github.Ptr("Deploy"),
						Status:     github.Ptr("completed"),
						Conclusion: github.Ptr("cancelled"),
					},
				},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "Deploy")
		assert.Contains(t, errorContent.Text, "cancelled")
		assert.NotContains(t, errorContent.Text, "Build")
	})

	t.Run("reports in-progress check run in combined failure block", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State:    github.Ptr("pending"),
				Statuses: []*github.RepoStatus{},
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(1),
				CheckRuns: []*github.CheckRun{
					{
						Name:   github.Ptr("Slow Test"),
						Status: github.Ptr("queued"),
					},
				},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "Slow Test")
		assert.Contains(t, errorContent.Text, "queued")
	})

	t.Run("reports both failing statuses and check runs in combined failure block", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("failure"),
				Statuses: []*github.RepoStatus{
					{
						Context: github.Ptr("ci/circleci"),
						State:   github.Ptr("failure"),
					},
					{
						Context: github.Ptr("ci/travis"),
						State:   github.Ptr("success"),
					},
				},
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(1),
				CheckRuns: []*github.CheckRun{
					{
						Name:       github.Ptr("CodeQL"),
						Status:     github.Ptr("completed"),
						Conclusion: github.Ptr("timed_out"),
					},
				},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		// Both failing status and failing check run reported
		assert.Contains(t, errorContent.Text, "ci/circleci")
		assert.Contains(t, errorContent.Text, "CodeQL")
		// Successful status not reported
		assert.NotContains(t, errorContent.Text, "ci/travis")
	})

	// === Post-gate individual check run validation (lines 172-188) ===

	t.Run("rejects merge when check run is incomplete despite success combined status", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(2),
				CheckRuns: []*github.CheckRun{
					{
						Name:       github.Ptr("Fast CI"),
						Status:     github.Ptr("completed"),
						Conclusion: github.Ptr("success"),
					},
					{
						Name:   github.Ptr("Slow Integration"),
						Status: github.Ptr("in_progress"),
					},
				},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "check runs are not passing")
		assert.Contains(t, errorContent.Text, "Slow Integration")
		assert.Contains(t, errorContent.Text, "status: in_progress")
	})

	t.Run("rejects merge when check run has stale conclusion despite success combined status", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(1),
				CheckRuns: []*github.CheckRun{
					{
						Name:       github.Ptr("Stale Check"),
						Status:     github.Ptr("completed"),
						Conclusion: github.Ptr("stale"),
					},
				},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "check runs are not passing")
		assert.Contains(t, errorContent.Text, "Stale Check")
		assert.Contains(t, errorContent.Text, "conclusion: stale")
	})

	// === Branch deletion error paths ===

	t.Run("returns error when branch deletion fails with 500", func(t *testing.T) {
		headSHA := "abc123def456"
		mergedSHA := "merged789xyz"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
				Title: github.Ptr("Add feature"),
				Body:  github.Ptr("Feature description"),
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total:     github.Ptr(0),
				CheckRuns: []*github.CheckRun{},
			}),
			PutReposPullsMergeByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequestMergeResult{
				Merged:  github.Ptr(true),
				Message: github.Ptr("Pull Request successfully merged"),
				SHA:     github.Ptr(mergedSHA),
			}),
			"DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}": mockResponse(t, http.StatusInternalServerError, map[string]string{
				"message": "internal server error",
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "failed to delete branch")
	})

	t.Run("returns error when branch deletion returns 404", func(t *testing.T) {
		headSHA := "abc123def456"
		mergedSHA := "merged789xyz"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
				Title: github.Ptr("Add feature"),
				Body:  github.Ptr("Feature description"),
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total:     github.Ptr(0),
				CheckRuns: []*github.CheckRun{},
			}),
			PutReposPullsMergeByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequestMergeResult{
				Merged:  github.Ptr(true),
				Message: github.Ptr("Pull Request successfully merged"),
				SHA:     github.Ptr(mergedSHA),
			}),
			"DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}": mockResponse(t, http.StatusNotFound, map[string]string{
				"message": "Reference does not exist",
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "failed to delete branch")
	})

	// === Merge behavior triangulation: different PR metadata flows through ===

	t.Run("uses PR title and body as defaults when custom commit params omitted", func(t *testing.T) {
		headSHA := "abc123def456"
		mergedSHA := "merged789xyz"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("bugfix-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("develop"),
				},
				Title: github.Ptr("fix: resolve null pointer"),
				Body:  github.Ptr("Fixes #99 by adding nil check"),
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total:     github.Ptr(0),
				CheckRuns: []*github.CheckRun{},
			}),
			PutReposPullsMergeByOwnerByRepoByPullNumber: expectRequestBody(t, map[string]any{
				"commit_title":   "fix: resolve null pointer",
				"commit_message": "Fixes #99 by adding nil check",
				"merge_method":   "squash",
			}).andThen(
				mockResponse(t, http.StatusOK, &github.PullRequestMergeResult{
					Merged:  github.Ptr(true),
					Message: github.Ptr("Pull Request successfully merged"),
					SHA:     github.Ptr(mergedSHA),
				}),
			),
			"DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}": mockResponse(t, http.StatusNoContent, nil),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(99),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var response map[string]any
		err = json.Unmarshal([]byte(textContent.Text), &response)
		require.NoError(t, err)
		assert.Equal(t, mergedSHA, response["mergedSHA"])
		assert.Equal(t, "bugfix-branch", response["branchName"])
		assert.Equal(t, "develop", response["baseBranch"])
	})

	// === PR state triangulation: different non-open states ===

	t.Run("returns error when PR is merged", func(t *testing.T) {
		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("merged"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr("abc123"),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "not open")
		assert.Contains(t, errorContent.Text, "merged")
		assert.Contains(t, errorContent.Text, fmt.Sprintf("#%d", 42))
	})

	// === Pending with check runs but no statuses ===

	t.Run("rejects merge when combined status is pending with check runs but no statuses", func(t *testing.T) {
		headSHA := "abc123def456"

		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				State: github.Ptr("open"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr(headSHA),
					Ref: github.Ptr("feature-branch"),
				},
				Base: &github.PullRequestBranch{
					Ref: github.Ptr("main"),
				},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State:    github.Ptr("pending"),
				Statuses: []*github.RepoStatus{},
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total: github.Ptr(1),
				CheckRuns: []*github.CheckRun{
					{
						Name:   github.Ptr("CI"),
						Status: github.Ptr("in_progress"),
					},
				},
			}),
		})

		client := github.NewClient(mockedClient)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":      "owner",
			"repo":       "repo",
			"pullNumber": float64(42),
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.True(t, result.IsError)

		errorContent := getErrorResult(t, result)
		assert.Contains(t, errorContent.Text, "cannot merge")
		assert.Contains(t, errorContent.Text, "CI")
	})
}

<<<<<<< HEAD
func openPRResponse(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		pr := &github.PullRequest{
			Number: github.Ptr(1),
			State:  github.Ptr("open"),
			Title:  github.Ptr("PR title"),
			Body:   github.Ptr("PR body"),
			Head: &github.PullRequestBranch{
				SHA: github.Ptr("abc123"),
				Ref: github.Ptr("feature-branch"),
			},
			Base: &github.PullRequestBranch{
				Ref: github.Ptr("main"),
			},
		}
		w.WriteHeader(http.StatusOK)
		b, _ := json.Marshal(pr)
		_, _ = w.Write(b)
	}
}

func passingStatusResponse(t *testing.T) http.HandlerFunc {
	t.Helper()
	return mockResponse(t, http.StatusOK, &github.CombinedStatus{
		State: github.Ptr("success"),
	})
}

func passingCheckRunsResponse(t *testing.T) http.HandlerFunc {
	t.Helper()
	return mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
		Total: github.Ptr(1),
		CheckRuns: []*github.CheckRun{{
			Status:     github.Ptr("completed"),
			Conclusion: github.Ptr("success"),
		}},
	})
}

func successfulMergeResponse(t *testing.T) http.HandlerFunc {
	t.Helper()
	mergeCount := 0
	return func(w http.ResponseWriter, _ *http.Request) {
		mergeCount++
		result := &github.PullRequestMergeResult{
			Merged:  github.Ptr(true),
			Message: github.Ptr("Pull Request successfully merged"),
			SHA:     github.Ptr(fmt.Sprintf("merged-sha-%d", mergeCount)),
		}
		w.WriteHeader(http.StatusOK)
		b, _ := json.Marshal(result)
		_, _ = w.Write(b)
	}
}

func updateBranchResponse(t *testing.T) http.HandlerFunc {
	t.Helper()
	return mockResponse(t, http.StatusAccepted, &github.PullRequestBranchUpdateResponse{
		Message: github.Ptr("Updating pull request branch."),
	})
}

func deleteRefHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func happyPathHandlers(t *testing.T) map[string]http.HandlerFunc {
	t.Helper()
	return map[string]http.HandlerFunc{
		GetReposPullsByOwnerByRepoByPullNumber:                openPRResponse(t),
		GetReposCommitsStatusByOwnerByRepoByRef:               passingStatusResponse(t),
		GetReposCommitsCheckRunsByOwnerByRepoByRef:            passingCheckRunsResponse(t),
		PutReposPullsMergeByOwnerByRepoByPullNumber:           successfulMergeResponse(t),
		PutReposPullsUpdateBranchByOwnerByRepoByPullNumber:    updateBranchResponse(t),
		"DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}":      deleteRefHandler(),
	}
}


func Test_MergePRStack(t *testing.T) {
	serverTool := MergePRStack(translations.NullTranslationHelper)
	tool := serverTool.Tool
	require.NoError(t, toolsnaps.Test(tool.Name, tool))

	assert.Equal(t, "merge_pr_stack", tool.Name)
	assert.NotEmpty(t, tool.Description)
	schema := tool.InputSchema.(*jsonschema.Schema)
	assert.Contains(t, schema.Properties, "owner")
	assert.Contains(t, schema.Properties, "repo")
	assert.Contains(t, schema.Properties, "pullNumbers")
	assert.Contains(t, schema.Properties, "deleteRemoteBranches")
	assert.ElementsMatch(t, schema.Required, []string{"owner", "repo", "pullNumbers"})

	t.Run("happy path merges 3 PRs in order", func(t *testing.T) {
		callCount := map[string]int{}

		handlers := happyPathHandlers(t)
		// Wrap handlers with counting
		origGetPR := handlers[GetReposPullsByOwnerByRepoByPullNumber]
		handlers[GetReposPullsByOwnerByRepoByPullNumber] = func(w http.ResponseWriter, r *http.Request) {
			callCount["get_pr"]++
			origGetPR(w, r)
		}
		origStatus := handlers[GetReposCommitsStatusByOwnerByRepoByRef]
		handlers[GetReposCommitsStatusByOwnerByRepoByRef] = func(w http.ResponseWriter, r *http.Request) {
			callCount["get_status"]++
			origStatus(w, r)
		}
		origCheckRuns := handlers[GetReposCommitsCheckRunsByOwnerByRepoByRef]
		handlers[GetReposCommitsCheckRunsByOwnerByRepoByRef] = func(w http.ResponseWriter, r *http.Request) {
			callCount["get_check_runs"]++
			origCheckRuns(w, r)
		}
		origMerge := handlers[PutReposPullsMergeByOwnerByRepoByPullNumber]
		handlers[PutReposPullsMergeByOwnerByRepoByPullNumber] = func(w http.ResponseWriter, r *http.Request) {
			callCount["merge"]++
			origMerge(w, r)
		}
		origUpdateBranch := handlers[PutReposPullsUpdateBranchByOwnerByRepoByPullNumber]
		handlers[PutReposPullsUpdateBranchByOwnerByRepoByPullNumber] = func(w http.ResponseWriter, r *http.Request) {
			callCount["update_branch"]++
			origUpdateBranch(w, r)
		}
		handlers["DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}"] = func(w http.ResponseWriter, _ *http.Request) {
			callCount["delete_ref"]++
			w.WriteHeader(http.StatusNoContent)
		}

		mockedClient := MockHTTPClientWithHandlers(handlers)
		client := github.NewClient(mockedClient)
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":                "owner",
			"repo":                 "repo",
			"pullNumbers":          []any{float64(1), float64(2), float64(3)},
			"deleteRemoteBranches": true,
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 3)
		for _, r := range results {
			assert.Equal(t, "merged", r.Status)
			assert.NotEmpty(t, r.MergedSHA)
			assert.Empty(t, r.Error)
		}

		assert.Equal(t, 3, callCount["get_pr"], "expected 3 PR fetches")
		assert.Equal(t, 3, callCount["merge"], "expected 3 merges")
		assert.Equal(t, 2, callCount["update_branch"], "expected 2 branch updates")
		assert.Equal(t, 3, callCount["delete_ref"], "expected 3 branch deletions")
	})

	t.Run("middle PR merge fails so first merged second fails third skipped", func(t *testing.T) {
		mergeCount := 0
		handlers := happyPathHandlers(t)
		handlers[PutReposPullsMergeByOwnerByRepoByPullNumber] = func(w http.ResponseWriter, _ *http.Request) {
			mergeCount++
			if mergeCount == 2 {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, _ = w.Write([]byte(`{"message": "Pull request is not mergeable"}`))
				return
			}
			result := &github.PullRequestMergeResult{
				Merged:  github.Ptr(true),
				Message: github.Ptr("Pull Request successfully merged"),
				SHA:     github.Ptr("merged-sha"),
			}
			w.WriteHeader(http.StatusOK)
			b, _ := json.Marshal(result)
			_, _ = w.Write(b)
		}

		client := github.NewClient(MockHTTPClientWithHandlers(handlers))
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{float64(1), float64(2), float64(3)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 3)
		assert.Equal(t, "merged", results[0].Status)
		assert.Equal(t, "failed", results[1].Status)
		assert.Contains(t, results[1].Error, "failed to merge PR")
		assert.Equal(t, "skipped", results[2].Status)
	})

	t.Run("single PR in stack merges normally", func(t *testing.T) {
		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				Number: github.Ptr(42),
				State:  github.Ptr("open"),
				Title:  github.Ptr("Single PR"),
				Body:   github.Ptr("body"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr("head-sha"),
					Ref: github.Ptr("feature"),
				},
				Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
			}),
			GetReposCommitsStatusByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.CombinedStatus{
				State: github.Ptr("success"),
			}),
			GetReposCommitsCheckRunsByOwnerByRepoByRef: mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
				Total:     github.Ptr(0),
				CheckRuns: []*github.CheckRun{},
			}),
			PutReposPullsMergeByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequestMergeResult{
				Merged:  github.Ptr(true),
				Message: github.Ptr("Merged"),
				SHA:     github.Ptr("merged-sha-42"),
			}),
			"DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}": func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			},
		})

		client := github.NewClient(mockedClient)
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{float64(42)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 1)
		assert.Equal(t, "merged", results[0].Status)
		assert.Equal(t, 42, results[0].PullNumber)
		assert.Equal(t, "merged-sha-42", results[0].MergedSHA)
	})

	t.Run("empty pullNumbers array returns empty result", func(t *testing.T) {
		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{})
		client := github.NewClient(mockedClient)
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)
		assert.Empty(t, results)
	})

	t.Run("PR not open fails that one and skips rest", func(t *testing.T) {
		mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, &github.PullRequest{
				Number: github.Ptr(10),
				State:  github.Ptr("closed"),
				Title:  github.Ptr("Closed PR"),
				Body:   github.Ptr("body"),
				Head: &github.PullRequestBranch{
					SHA: github.Ptr("head-sha"),
					Ref: github.Ptr("feature"),
				},
				Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
			}),
		})

		client := github.NewClient(mockedClient)
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{float64(10), float64(11), float64(12)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 3)
		assert.Equal(t, "failed", results[0].Status)
		assert.Contains(t, results[0].Error, "not open")
		assert.Equal(t, "skipped", results[1].Status)
		assert.Equal(t, "skipped", results[2].Status)
	})

	// --- Parameter validation error paths ---

	t.Run("missing owner parameter returns error", func(t *testing.T) {
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"repo":        "repo",
			"pullNumbers": []any{float64(1)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getErrorResult(t, result)
		assert.Contains(t, textContent.Text, "owner")
	})

	t.Run("missing repo parameter returns error", func(t *testing.T) {
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"pullNumbers": []any{float64(1)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getErrorResult(t, result)
		assert.Contains(t, textContent.Text, "repo")
	})

	t.Run("missing pullNumbers parameter returns error", func(t *testing.T) {
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner": "owner",
			"repo":  "repo",
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getErrorResult(t, result)
		assert.Contains(t, textContent.Text, "pullNumbers")
	})

	t.Run("pullNumbers not an array returns error", func(t *testing.T) {
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": "not-an-array",
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getErrorResult(t, result)
		assert.Contains(t, textContent.Text, "pullNumbers must be an array")
	})

	t.Run("pullNumbers containing non-number returns error", func(t *testing.T) {
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{"not-a-number"},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getErrorResult(t, result)
		assert.Contains(t, textContent.Text, "pullNumbers must contain numbers")
	})

	t.Run("deleteRemoteBranches wrong type returns error", func(t *testing.T) {
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":                "owner",
			"repo":                 "repo",
			"pullNumbers":          []any{float64(1)},
			"deleteRemoteBranches": "not-a-bool",
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getErrorResult(t, result)
		assert.Contains(t, textContent.Text, "deleteRemoteBranches")
	})

	t.Run("GetClient error returns tool error", func(t *testing.T) {
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := stubDeps{clientFn: stubClientFnErr("auth token expired")}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{float64(1)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getErrorResult(t, result)
		assert.Contains(t, textContent.Text, "failed to get GitHub client")
		assert.Contains(t, textContent.Text, "auth token expired")
	})

	// --- PR fetch error ---

	t.Run("PR fetch API error fails and skips remaining", func(t *testing.T) {
		handlers := map[string]http.HandlerFunc{
			GetReposPullsByOwnerByRepoByPullNumber: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message": "Not Found"}`))
			},
		}

		client := github.NewClient(MockHTTPClientWithHandlers(handlers))
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{float64(999), float64(1000)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 2)
		assert.Equal(t, "failed", results[0].Status)
		assert.Contains(t, results[0].Error, "failed to get PR #999")
		assert.Equal(t, "skipped", results[1].Status)
	})

	// --- checksPass coverage ---

	t.Run("combined status failure causes PR to fail", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsStatusByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.CombinedStatus{
			State: github.Ptr("failure"),
		})

		client := github.NewClient(MockHTTPClientWithHandlers(handlers))
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{float64(1), float64(2)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 2)
		assert.Equal(t, "failed", results[0].Status)
		assert.Contains(t, results[0].Error, "failing checks")
		assert.Equal(t, "skipped", results[1].Status)
	})

	t.Run("combined status error causes PR to fail", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsStatusByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.CombinedStatus{
			State: github.Ptr("error"),
		})

		client := github.NewClient(MockHTTPClientWithHandlers(handlers))
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{float64(5)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 1)
		assert.Equal(t, "failed", results[0].Status)
		assert.Contains(t, results[0].Error, "failing checks")
	})

	t.Run("GetCombinedStatus API error causes PR to fail", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsStatusByOwnerByRepoByRef] = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message": "Internal Server Error"}`))
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
		assert.Equal(t, "failed", results[0].Status)
		assert.Contains(t, results[0].Error, "failing checks")
	})

	t.Run("ListCheckRunsForRef API error causes PR to fail", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsCheckRunsByOwnerByRepoByRef] = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message": "Internal Server Error"}`))
		}

		client := github.NewClient(MockHTTPClientWithHandlers(handlers))
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{float64(1), float64(2)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 2)
		assert.Equal(t, "failed", results[0].Status)
		assert.Contains(t, results[0].Error, "failing checks")
		assert.Equal(t, "skipped", results[1].Status)
	})

	t.Run("check run with cancelled conclusion causes PR to fail", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsCheckRunsByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
			Total: github.Ptr(1),
			CheckRuns: []*github.CheckRun{
				{Conclusion: github.Ptr("cancelled")},
			},
		})

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
		assert.Equal(t, "failed", results[0].Status)
		assert.Contains(t, results[0].Error, "failing checks")
	})

	t.Run("check run with timed_out conclusion causes PR to fail", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsCheckRunsByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
			Total: github.Ptr(1),
			CheckRuns: []*github.CheckRun{
				{Conclusion: github.Ptr("timed_out")},
			},
		})

		client := github.NewClient(MockHTTPClientWithHandlers(handlers))
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{float64(7)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 1)
		assert.Equal(t, "failed", results[0].Status)
	})

	t.Run("check run with action_required conclusion causes PR to fail", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsCheckRunsByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
			Total: github.Ptr(1),
			CheckRuns: []*github.CheckRun{
				{Conclusion: github.Ptr("action_required")},
			},
		})

		client := github.NewClient(MockHTTPClientWithHandlers(handlers))
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{float64(8)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 1)
		assert.Equal(t, "failed", results[0].Status)
	})

	t.Run("check runs with neutral and skipped conclusions pass", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsCheckRunsByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
			Total: github.Ptr(3),
			CheckRuns: []*github.CheckRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("neutral")},
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("skipped")},
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("success")},
			},
		})

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
		assert.Equal(t, "merged", results[0].Status)
	})

	// --- Triangulation: stack ordering with 2 PRs ---

	t.Run("2-PR stack merges both with one branch update", func(t *testing.T) {
		updateBranchCount := 0
		handlers := happyPathHandlers(t)
		origUpdate := handlers[PutReposPullsUpdateBranchByOwnerByRepoByPullNumber]
		handlers[PutReposPullsUpdateBranchByOwnerByRepoByPullNumber] = func(w http.ResponseWriter, r *http.Request) {
			updateBranchCount++
			origUpdate(w, r)
		}

		client := github.NewClient(MockHTTPClientWithHandlers(handlers))
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{float64(10), float64(20)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 2)
		assert.Equal(t, "merged", results[0].Status)
		assert.Equal(t, "merged", results[1].Status)
		assert.Equal(t, 1, updateBranchCount, "expected exactly 1 branch update between 2 PRs")
	})

	// --- Triangulation: first PR fails, all others skipped ---

	t.Run("first PR in stack fails so all remaining are skipped", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsCheckRunsByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
			Total: github.Ptr(1),
			CheckRuns: []*github.CheckRun{
				{Conclusion: github.Ptr("failure")},
			},
		})

		client := github.NewClient(MockHTTPClientWithHandlers(handlers))
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{float64(1), float64(2), float64(3)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 3)
		assert.Equal(t, "failed", results[0].Status)
		assert.Contains(t, results[0].Error, "failing checks")
		assert.Equal(t, "skipped", results[1].Status)
		assert.Equal(t, "skipped", results[2].Status)
	})

	// --- Triangulation: last PR fails, first N-1 merged ---

	t.Run("last PR in 3-stack fails so first two merged", func(t *testing.T) {
		mergeCount := 0
		handlers := happyPathHandlers(t)
		handlers[PutReposPullsMergeByOwnerByRepoByPullNumber] = func(w http.ResponseWriter, _ *http.Request) {
			mergeCount++
			if mergeCount == 3 {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"message": "merge conflict"}`))
				return
			}
			result := &github.PullRequestMergeResult{
				Merged:  github.Ptr(true),
				Message: github.Ptr("merged"),
				SHA:     github.Ptr(fmt.Sprintf("sha-%d", mergeCount)),
			}
			w.WriteHeader(http.StatusOK)
			b, _ := json.Marshal(result)
			_, _ = w.Write(b)
		}

		client := github.NewClient(MockHTTPClientWithHandlers(handlers))
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":       "owner",
			"repo":        "repo",
			"pullNumbers": []any{float64(1), float64(2), float64(3)},
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 3)
		assert.Equal(t, "merged", results[0].Status)
		assert.Equal(t, "sha-1", results[0].MergedSHA)
		assert.Equal(t, "merged", results[1].Status)
		assert.Equal(t, "sha-2", results[1].MergedSHA)
		assert.Equal(t, "failed", results[2].Status)
		assert.Contains(t, results[2].Error, "failed to merge PR")
	})

	// --- deleteRemoteBranches=false skips branch deletion ---

	t.Run("deleteRemoteBranches false skips branch deletion", func(t *testing.T) {
		deleteRefCalled := false
		handlers := happyPathHandlers(t)
		handlers["DELETE /repos/{owner}/{repo}/git/refs/{ref:.*}"] = func(w http.ResponseWriter, _ *http.Request) {
			deleteRefCalled = true
			w.WriteHeader(http.StatusNoContent)
		}

		client := github.NewClient(MockHTTPClientWithHandlers(handlers))
		serverTool := MergePRStack(translations.NullTranslationHelper)
		deps := BaseDeps{Client: client}
		handler := serverTool.Handler(deps)

		request := createMCPRequest(map[string]any{
			"owner":                "owner",
			"repo":                 "repo",
			"pullNumbers":          []any{float64(1)},
			"deleteRemoteBranches": false,
		})

		result, err := handler(ContextWithDeps(context.Background(), deps), &request)
		require.NoError(t, err)
		require.False(t, result.IsError)

		textContent := getTextResult(t, result)
		var results []MergePRStackResult
		err = json.Unmarshal([]byte(textContent.Text), &results)
		require.NoError(t, err)

		require.Len(t, results, 1)
		assert.Equal(t, "merged", results[0].Status)
		assert.False(t, deleteRefCalled, "branch deletion handler was called despite deleteRemoteBranches=false")
	})

	// --- Merge API error with non-nil error (distinct from status-code-only failure) ---

	t.Run("merge API returns error object with non-200 status", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[PutReposPullsMergeByOwnerByRepoByPullNumber] = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message": "Head branch was modified"}`))
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
		assert.Equal(t, "failed", results[0].Status)
		assert.Contains(t, results[0].Error, "failed to merge PR #1")
	})

	// --- Combined status pending passes (not failure/error) ---

	t.Run("combined status pending with check runs rejects merge", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsStatusByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.CombinedStatus{
			State: github.Ptr("pending"),
		})

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
		assert.Equal(t, "failed", results[0].Status)
		assert.Contains(t, results[0].Error, "failing checks")
	})

	// --- No check runs at all passes ---

	t.Run("no check runs with success status passes", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsCheckRunsByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
			Total:     github.Ptr(0),
			CheckRuns: []*github.CheckRun{},
		})

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
		assert.Equal(t, "merged", results[0].Status)
	})

	// --- Allow-list CI gate: in_progress check run (the critical bug) ---

	t.Run("check run with in_progress status and no conclusion rejects merge", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsCheckRunsByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
			Total: github.Ptr(1),
			CheckRuns: []*github.CheckRun{
				{Name: github.Ptr("ci-build"), Status: github.Ptr("in_progress")},
			},
		})

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
		assert.Equal(t, "failed", results[0].Status)
		assert.Contains(t, results[0].Error, "failing checks")
	})

	// --- Allow-list CI gate: neutral conclusion with completed status passes ---

	t.Run("check run with completed status and neutral conclusion passes", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsCheckRunsByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
			Total: github.Ptr(1),
			CheckRuns: []*github.CheckRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("neutral")},
			},
		})

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
		assert.Equal(t, "merged", results[0].Status)
	})

	// --- Allow-list CI gate: skipped conclusion with completed status passes ---

	t.Run("check run with completed status and skipped conclusion passes", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsCheckRunsByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
			Total: github.Ptr(1),
			CheckRuns: []*github.CheckRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("skipped")},
			},
		})

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
		assert.Equal(t, "merged", results[0].Status)
	})

	// --- Allow-list CI gate: pending status with no CI configured passes ---

	t.Run("combined status pending with zero statuses and zero check runs passes", func(t *testing.T) {
		handlers := happyPathHandlers(t)
		handlers[GetReposCommitsStatusByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.CombinedStatus{
			State:    github.Ptr("pending"),
			Statuses: []*github.RepoStatus{},
		})
		handlers[GetReposCommitsCheckRunsByOwnerByRepoByRef] = mockResponse(t, http.StatusOK, &github.ListCheckRunsResults{
			Total:     github.Ptr(0),
			CheckRuns: []*github.CheckRun{},
		})

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
		assert.Equal(t, "merged", results[0].Status)
	})
}

func Test_BatchPRStatus(t *testing.T) {
	// Verify tool definition
	serverTool := BatchPRStatus(translations.NullTranslationHelper)
	tool := serverTool.Tool
	require.NoError(t, toolsnaps.Test(tool.Name, tool))

	assert.Equal(t, "batch_pr_status", tool.Name)
	assert.NotEmpty(t, tool.Description)
	schema := tool.InputSchema.(*jsonschema.Schema)
	assert.Contains(t, schema.Properties, "owner")
	assert.Contains(t, schema.Properties, "repo")
	assert.Contains(t, schema.Properties, "state")
	assert.Contains(t, schema.Properties, "labels")
	assert.Contains(t, schema.Properties, "head")
	assert.ElementsMatch(t, schema.Required, []string{"owner", "repo"})

	// Verify annotations
	require.NotNil(t, tool.Annotations)
	assert.True(t, tool.Annotations.ReadOnlyHint)

	now := time.Now().UTC()
	yesterday := now.Add(-24 * time.Hour)

	tests := []struct {
		name         string
		mockedClient *http.Client
		requestArgs  map[string]any
		expectError  bool
		expectedLen  int
		validate     func(t *testing.T, results []BatchPRStatusResult)
	}{
		{
			name: "returns statuses for 3 open PRs with different CI and review states",
			mockedClient: MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
				GetReposPullsByOwnerByRepo: mockResponse(t, http.StatusOK, []*github.PullRequest{
					{
						Number:       github.Ptr(1),
						Title:        github.Ptr("feat: add auth"),
						Draft:        github.Ptr(false),
						Mergeable:    github.Ptr(true),
						Additions:    github.Ptr(150),
						Deletions:    github.Ptr(30),
						ChangedFiles: github.Ptr(5),
						CreatedAt:    &github.Timestamp{Time: yesterday},
						UpdatedAt:    &github.Timestamp{Time: now},
						Body:         github.Ptr("Closes #41"),
						User:         &github.User{Login: github.Ptr("alice")},
						Head: &github.PullRequestBranch{
							Ref: github.Ptr("issue-41-auth"),
							SHA: github.Ptr("sha1"),
						},
						Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
					},
					{
						Number:       github.Ptr(2),
						Title:        github.Ptr("fix: broken tests"),
						Draft:        github.Ptr(false),
						Mergeable:    github.Ptr(false),
						Additions:    github.Ptr(10),
						Deletions:    github.Ptr(5),
						ChangedFiles: github.Ptr(2),
						CreatedAt:    &github.Timestamp{Time: yesterday},
						UpdatedAt:    &github.Timestamp{Time: now},
						Body:         github.Ptr("No linked issues"),
						User:         &github.User{Login: github.Ptr("bob")},
						Head: &github.PullRequestBranch{
							Ref: github.Ptr("fix-tests"),
							SHA: github.Ptr("sha2"),
						},
						Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
					},
					{
						Number:       github.Ptr(3),
						Title:        github.Ptr("chore: update deps"),
						Draft:        github.Ptr(true),
						Mergeable:    github.Ptr(true),
						Additions:    github.Ptr(200),
						Deletions:    github.Ptr(100),
						ChangedFiles: github.Ptr(1),
						CreatedAt:    &github.Timestamp{Time: yesterday},
						UpdatedAt:    &github.Timestamp{Time: now},
						Body:         github.Ptr(""),
						User:         &github.User{Login: github.Ptr("carol")},
						Head: &github.PullRequestBranch{
							Ref: github.Ptr("update-deps"),
							SHA: github.Ptr("sha3"),
						},
						Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
					},
				}),
				// Combined status for PR #1 (passing)
				"GET /repos/owner/repo/commits/sha1/status": mockResponse(t, http.StatusOK, &github.CombinedStatus{
					State: github.Ptr("success"),
				}),
				// Combined status for PR #2 (failing)
				"GET /repos/owner/repo/commits/sha2/status": mockResponse(t, http.StatusOK, &github.CombinedStatus{
					State: github.Ptr("failure"),
				}),
				// Combined status for PR #3 (pending)
				"GET /repos/owner/repo/commits/sha3/status": mockResponse(t, http.StatusOK, &github.CombinedStatus{
					State: github.Ptr("pending"),
				}),
				// Reviews for PR #1 (approved)
				"GET /repos/owner/repo/pulls/1/reviews": mockResponse(t, http.StatusOK, []*github.PullRequestReview{
					{User: &github.User{Login: github.Ptr("reviewer1")}, State: github.Ptr("APPROVED")},
				}),
				// Reviews for PR #2 (changes requested)
				"GET /repos/owner/repo/pulls/2/reviews": mockResponse(t, http.StatusOK, []*github.PullRequestReview{
					{User: &github.User{Login: github.Ptr("reviewer2")}, State: github.Ptr("CHANGES_REQUESTED")},
				}),
				// Reviews for PR #3 (no reviews)
				"GET /repos/owner/repo/pulls/3/reviews": mockResponse(t, http.StatusOK, []*github.PullRequestReview{}),
			}),
			requestArgs: map[string]any{
				"owner": "owner",
				"repo":  "repo",
			},
			expectError: false,
			expectedLen: 3,
			validate: func(t *testing.T, results []BatchPRStatusResult) {
				t.Helper()

				// PR #1: passing CI, approved
				assert.Equal(t, 1, results[0].Number)
				assert.Equal(t, "feat: add auth", results[0].Title)
				assert.Equal(t, "alice", results[0].Author)
				assert.Equal(t, "issue-41-auth", results[0].Branch)
				assert.Equal(t, "main", results[0].BaseBranch)
				assert.Equal(t, "success", results[0].CI)
				assert.Equal(t, "approved", results[0].ReviewStatus)
				assert.Equal(t, []string{"reviewer1"}, results[0].Reviewers)
				assert.Equal(t, []int{41}, results[0].LinkedIssues)
				assert.True(t, results[0].Mergeable)
				assert.False(t, results[0].Draft)
				assert.Equal(t, 150, results[0].Additions)
				assert.Equal(t, 30, results[0].Deletions)
				assert.Equal(t, 5, results[0].ChangedFiles)

				// PR #2: failing CI, changes requested
				assert.Equal(t, 2, results[1].Number)
				assert.Equal(t, "failure", results[1].CI)
				assert.Equal(t, "changes_requested", results[1].ReviewStatus)
				assert.Equal(t, []string{"reviewer2"}, results[1].Reviewers)
				assert.Empty(t, results[1].LinkedIssues)
				assert.False(t, results[1].Mergeable)

				// PR #3: pending CI, no reviews, draft
				assert.Equal(t, 3, results[2].Number)
				assert.Equal(t, "pending", results[2].CI)
				assert.Equal(t, "none", results[2].ReviewStatus)
				assert.Empty(t, results[2].Reviewers)
				assert.Empty(t, results[2].LinkedIssues)
				assert.True(t, results[2].Draft)
			},
		},
		{
			name: "returns empty array when no open PRs exist",
			mockedClient: MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
				GetReposPullsByOwnerByRepo: mockResponse(t, http.StatusOK, []*github.PullRequest{}),
			}),
			requestArgs: map[string]any{
				"owner": "owner",
				"repo":  "repo",
			},
			expectError: false,
			expectedLen: 0,
			validate: func(t *testing.T, results []BatchPRStatusResult) {
				t.Helper()
				assert.Empty(t, results)
			},
		},
		{
			name: "filters by labels when provided",
			mockedClient: MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
				GetReposPullsByOwnerByRepo: expectQueryParams(t, map[string]string{
					"state":    "open",
					"per_page": "30",
				}).andThen(mockResponse(t, http.StatusOK, []*github.PullRequest{
					{
						Number:       github.Ptr(10),
						Title:        github.Ptr("labeled PR"),
						Draft:        github.Ptr(false),
						Mergeable:    github.Ptr(true),
						Additions:    github.Ptr(5),
						Deletions:    github.Ptr(2),
						ChangedFiles: github.Ptr(1),
						CreatedAt:    &github.Timestamp{Time: yesterday},
						UpdatedAt:    &github.Timestamp{Time: now},
						Body:         github.Ptr(""),
						User:         &github.User{Login: github.Ptr("dev")},
						Labels: []*github.Label{
							{Name: github.Ptr("bug")},
						},
						Head: &github.PullRequestBranch{
							Ref: github.Ptr("fix-bug"),
							SHA: github.Ptr("shaX"),
						},
						Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
					},
					{
						Number:       github.Ptr(11),
						Title:        github.Ptr("unlabeled PR"),
						Draft:        github.Ptr(false),
						Mergeable:    github.Ptr(true),
						Additions:    github.Ptr(1),
						Deletions:    github.Ptr(1),
						ChangedFiles: github.Ptr(1),
						CreatedAt:    &github.Timestamp{Time: yesterday},
						UpdatedAt:    &github.Timestamp{Time: now},
						Body:         github.Ptr(""),
						User:         &github.User{Login: github.Ptr("dev2")},
						Labels:       []*github.Label{},
						Head: &github.PullRequestBranch{
							Ref: github.Ptr("other"),
							SHA: github.Ptr("shaY"),
						},
						Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
					},
				})),
				"GET /repos/owner/repo/commits/shaX/status": mockResponse(t, http.StatusOK, &github.CombinedStatus{
					State: github.Ptr("success"),
				}),
				"GET /repos/owner/repo/pulls/10/reviews": mockResponse(t, http.StatusOK, []*github.PullRequestReview{}),
			}),
			requestArgs: map[string]any{
				"owner":  "owner",
				"repo":   "repo",
				"labels": []any{"bug"},
			},
			expectError: false,
			expectedLen: 1,
			validate: func(t *testing.T, results []BatchPRStatusResult) {
				t.Helper()
				assert.Equal(t, 10, results[0].Number)
				assert.Equal(t, "labeled PR", results[0].Title)
			},
		},
		{
			name: "parses Closes and Fixes patterns from PR body",
			mockedClient: MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
				GetReposPullsByOwnerByRepo: mockResponse(t, http.StatusOK, []*github.PullRequest{
					{
						Number:       github.Ptr(20),
						Title:        github.Ptr("multi-close PR"),
						Draft:        github.Ptr(false),
						Mergeable:    github.Ptr(true),
						Additions:    github.Ptr(50),
						Deletions:    github.Ptr(10),
						ChangedFiles: github.Ptr(3),
						CreatedAt:    &github.Timestamp{Time: yesterday},
						UpdatedAt:    &github.Timestamp{Time: now},
						Body:         github.Ptr("Closes #5, Fixes #10, Resolves #15"),
						User:         &github.User{Login: github.Ptr("dev")},
						Head: &github.PullRequestBranch{
							Ref: github.Ptr("multi-fix"),
							SHA: github.Ptr("shaM"),
						},
						Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
					},
				}),
				"GET /repos/owner/repo/commits/shaM/status": mockResponse(t, http.StatusOK, &github.CombinedStatus{
					State: github.Ptr("success"),
				}),
				"GET /repos/owner/repo/pulls/20/reviews": mockResponse(t, http.StatusOK, []*github.PullRequestReview{}),
			}),
			requestArgs: map[string]any{
				"owner": "owner",
				"repo":  "repo",
			},
			expectError: false,
			expectedLen: 1,
			validate: func(t *testing.T, results []BatchPRStatusResult) {
				t.Helper()
				assert.ElementsMatch(t, []int{5, 10, 15}, results[0].LinkedIssues)
			},
		},
		{
			name: "marks draft PRs correctly",
			mockedClient: MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
				GetReposPullsByOwnerByRepo: mockResponse(t, http.StatusOK, []*github.PullRequest{
					{
						Number:       github.Ptr(30),
						Title:        github.Ptr("draft PR"),
						Draft:        github.Ptr(true),
						Mergeable:    github.Ptr(false),
						Additions:    github.Ptr(1),
						Deletions:    github.Ptr(0),
						ChangedFiles: github.Ptr(1),
						CreatedAt:    &github.Timestamp{Time: yesterday},
						UpdatedAt:    &github.Timestamp{Time: now},
						Body:         github.Ptr("WIP"),
						User:         &github.User{Login: github.Ptr("dev")},
						Head: &github.PullRequestBranch{
							Ref: github.Ptr("wip-branch"),
							SHA: github.Ptr("shaD"),
						},
						Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
					},
				}),
				"GET /repos/owner/repo/commits/shaD/status": mockResponse(t, http.StatusOK, &github.CombinedStatus{
					State: github.Ptr("pending"),
				}),
				"GET /repos/owner/repo/pulls/30/reviews": mockResponse(t, http.StatusOK, []*github.PullRequestReview{}),
			}),
			requestArgs: map[string]any{
				"owner": "owner",
				"repo":  "repo",
			},
			expectError: false,
			expectedLen: 1,
			validate: func(t *testing.T, results []BatchPRStatusResult) {
				t.Helper()
				assert.True(t, results[0].Draft)
			},
		},
		{
			name: "handles mixed CI statuses correctly",
			mockedClient: MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
				GetReposPullsByOwnerByRepo: mockResponse(t, http.StatusOK, []*github.PullRequest{
					{
						Number:       github.Ptr(40),
						Title:        github.Ptr("passing"),
						Draft:        github.Ptr(false),
						Mergeable:    github.Ptr(true),
						Additions:    github.Ptr(10),
						Deletions:    github.Ptr(5),
						ChangedFiles: github.Ptr(2),
						CreatedAt:    &github.Timestamp{Time: yesterday},
						UpdatedAt:    &github.Timestamp{Time: now},
						Body:         github.Ptr(""),
						User:         &github.User{Login: github.Ptr("dev")},
						Head: &github.PullRequestBranch{
							Ref: github.Ptr("branch-a"),
							SHA: github.Ptr("shaA"),
						},
						Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
					},
					{
						Number:       github.Ptr(41),
						Title:        github.Ptr("failing"),
						Draft:        github.Ptr(false),
						Mergeable:    github.Ptr(false),
						Additions:    github.Ptr(20),
						Deletions:    github.Ptr(10),
						ChangedFiles: github.Ptr(3),
						CreatedAt:    &github.Timestamp{Time: yesterday},
						UpdatedAt:    &github.Timestamp{Time: now},
						Body:         github.Ptr(""),
						User:         &github.User{Login: github.Ptr("dev2")},
						Head: &github.PullRequestBranch{
							Ref: github.Ptr("branch-b"),
							SHA: github.Ptr("shaB"),
						},
						Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
					},
					{
						Number:       github.Ptr(42),
						Title:        github.Ptr("pending"),
						Draft:        github.Ptr(false),
						Mergeable:    github.Ptr(true),
						Additions:    github.Ptr(5),
						Deletions:    github.Ptr(2),
						ChangedFiles: github.Ptr(1),
						CreatedAt:    &github.Timestamp{Time: yesterday},
						UpdatedAt:    &github.Timestamp{Time: now},
						Body:         github.Ptr(""),
						User:         &github.User{Login: github.Ptr("dev3")},
						Head: &github.PullRequestBranch{
							Ref: github.Ptr("branch-c"),
							SHA: github.Ptr("shaC"),
						},
						Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
					},
				}),
				"GET /repos/owner/repo/commits/shaA/status": mockResponse(t, http.StatusOK, &github.CombinedStatus{
					State: github.Ptr("success"),
				}),
				"GET /repos/owner/repo/commits/shaB/status": mockResponse(t, http.StatusOK, &github.CombinedStatus{
					State: github.Ptr("failure"),
				}),
				"GET /repos/owner/repo/commits/shaC/status": mockResponse(t, http.StatusOK, &github.CombinedStatus{
					State: github.Ptr("pending"),
				}),
				"GET /repos/owner/repo/pulls/40/reviews": mockResponse(t, http.StatusOK, []*github.PullRequestReview{}),
				"GET /repos/owner/repo/pulls/41/reviews": mockResponse(t, http.StatusOK, []*github.PullRequestReview{}),
				"GET /repos/owner/repo/pulls/42/reviews": mockResponse(t, http.StatusOK, []*github.PullRequestReview{}),
			}),
			requestArgs: map[string]any{
				"owner": "owner",
				"repo":  "repo",
			},
			expectError: false,
			expectedLen: 3,
			validate: func(t *testing.T, results []BatchPRStatusResult) {
				t.Helper()
				assert.Equal(t, "success", results[0].CI)
				assert.Equal(t, "failure", results[1].CI)
				assert.Equal(t, "pending", results[2].CI)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := github.NewClient(tc.mockedClient)
			gqlClient := githubv4.NewClient(githubv4mock.NewMockedHTTPClient())
			deps := BaseDeps{
				Client:          client,
				GQLClient:       gqlClient,
				RepoAccessCache: stubRepoAccessCache(gqlClient, 5*time.Minute),
				Flags:           stubFeatureFlags(map[string]bool{"lockdown-mode": false}),
			}
			handler := serverTool.Handler(deps)

			request := createMCPRequest(tc.requestArgs)

			result, err := handler(ContextWithDeps(context.Background(), deps), &request)

			if tc.expectError {
				require.NoError(t, err)
				require.True(t, result.IsError)
				return
			}

			require.NoError(t, err)
			require.False(t, result.IsError)

			textContent := getTextResult(t, result)

			var results []BatchPRStatusResult
			err = json.Unmarshal([]byte(textContent.Text), &results)
			require.NoError(t, err)
			assert.Len(t, results, tc.expectedLen)

			if tc.validate != nil {
				tc.validate(t, results)
			}
		})
	}
}
