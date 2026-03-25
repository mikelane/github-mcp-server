package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/github/github-mcp-server/internal/toolsnaps"
	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/google/go-github/v82/github"
	"github.com/google/jsonschema-go/jsonschema"
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
