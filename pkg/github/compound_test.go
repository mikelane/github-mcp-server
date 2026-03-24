package github

import (
	"context"
	"encoding/json"
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
		assert.Contains(t, errorContent.Text, "failing")
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
}
