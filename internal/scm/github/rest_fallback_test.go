package github

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// graphQLRefusal is what a proxy that denies GraphQL but allows REST prints.
const graphQLRefusal = "HTTP 403: GitHub GraphQL is not available from Claude Code sessions; use the REST API (gh api repos/{owner}/{repo}/...). (https://api.github.com/graphql)\n"

func TestAvailableAcceptsATokenThatOnlyRESTCanValidate(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh auth status --hostname github.com":           {stderr: "github.com\n  X Failed to log in to github.com using token (GH_TOKEN)\n  - The token in GH_TOKEN is invalid.\n", code: 1},
		"gh api --hostname github.com --method GET user": {stdout: `{"login":"someone"}`},
	}), func() bool { return true }, "github.com", "test/repo")

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil when the token works over REST", err)
	}
}

func TestAvailableStillReportsATokenThatRESTRejectsToo(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh auth status":           {stderr: "github.com\n  X Failed to log in\n", code: 1},
		"gh api --method GET user": {stderr: "HTTP 401: Bad credentials\n", code: 1},
	}), func() bool { return true }, "", "test/repo")

	err := host.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("Available() error = %v, want not authenticated", err)
	}
}

func TestPRLifecycleFallsBackToRESTWhenGraphQLIsRefused(t *testing.T) {
	t.Parallel()

	createBody := `{"base":"main","body":"the body","draft":true,"head":"forker:feature","title":"the title"}`
	updateBody := `{"body":"new body","title":"new title"}`
	host := NewWithFork(githubTestCmdFactory(map[string]githubTestResponse{
		// The first GraphQL-backed call is refused; nothing GraphQL-backed may run after it.
		"gh pr list --head feature --base main --repo test/repo --state open --json number,url,baseRefName,headRefName,headRepositoryOwner": {stderr: graphQLRefusal, code: 1},
		"gh api --method GET repos/test/repo/pulls -f state=open -f head=forker:feature -f per_page=100 -f base=main":                       {stdout: "[]\n"},
		"gh api --method POST repos/test/repo/pulls --input -": {
			wantStdin: createBody,
			stdout:    `{"number":7,"html_url":"https://github.com/test/repo/pull/7"}`,
		},
		"gh api --method PATCH repos/test/repo/pulls/7 --input -": {wantStdin: updateBody, stdout: `{"number":7}`},
		"gh api --method GET repos/test/repo/pulls/7":             {stdout: `{"number":7,"html_url":"https://github.com/test/repo/pull/7","state":"closed","merged_at":"2026-10-05T00:00:00Z","title":"new title","body":null,"mergeable":null,"base":{"ref":"main"},"head":{"ref":"feature","sha":"abc123"}}`},
	}), func() bool { return true }, "", "test/repo", "forker/repo", true)
	ctx := context.Background()

	found, err := host.FindPR(ctx, "feature", "main")
	if err != nil || found != nil {
		t.Fatalf("FindPR() = %v, %v; want no PR and no error", found, err)
	}
	pr, err := host.CreatePR(ctx, "feature", "main", scm.PRContent{Title: "the title", Body: "the body"})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr.Number != "7" || pr.URL != "https://github.com/test/repo/pull/7" {
		t.Fatalf("CreatePR() = %+v, want PR 7", pr)
	}
	if _, err := host.UpdatePR(ctx, pr, scm.PRContent{Title: "new title", Body: "new body"}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	content, err := host.GetPRContent(ctx, pr)
	if err != nil || content.Title != "new title" || content.Body != "" {
		t.Fatalf("GetPRContent() = %+v, %v; want the title and an empty body for a null REST body", content, err)
	}
	if state, err := host.GetPRState(ctx, pr); err != nil || state != scm.PRStateMerged {
		t.Fatalf("GetPRState() = %q, %v; want MERGED", state, err)
	}
	if base, err := host.GetPRBaseBranch(ctx, pr); err != nil || base != "main" {
		t.Fatalf("GetPRBaseBranch() = %q, %v; want main", base, err)
	}
	if state, err := host.GetMergeableState(ctx, pr); err != nil || state != scm.MergeablePending {
		t.Fatalf("GetMergeableState() = %q, %v; want PENDING while GitHub is computing it", state, err)
	}
}

func TestFindPRFallsBackToRESTAndMatchesTheHeadOwner(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr list --head feature --repo test/repo --state open --json number,url,baseRefName": {stderr: graphQLRefusal, code: 1},
		"gh api --method GET repos/test/repo/pulls -f state=open -f head=test:feature -f per_page=100": {stdout: `[
			{"number":3,"html_url":"https://github.com/test/repo/pull/3","base":{"ref":"main"},"head":{"ref":"feature","repo":{"owner":{"login":"someone-else"}}}},
			{"number":4,"html_url":"https://github.com/test/repo/pull/4","base":{"ref":"main"},"head":{"ref":"feature","repo":{"owner":{"login":"test"}}}}
		]`},
	}), func() bool { return true }, "", "test/repo")

	pr, err := host.FindPR(context.Background(), "feature", "")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil || pr.Number != "4" || pr.BaseBranch != "main" {
		t.Fatalf("FindPR() = %+v, want PR 4 from the repository owner", pr)
	}
}

func TestOrdinaryGHFailuresDoNotFallBackToREST(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr list --head feature --repo test/repo --state open --json number,url,baseRefName": {stderr: "HTTP 401: Bad credentials (https://api.github.com/graphql)\n", code: 1},
	}), func() bool { return true }, "", "test/repo")

	_, err := host.FindPR(context.Background(), "feature", "")
	if err == nil || !strings.Contains(err.Error(), "gh pr list") || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("FindPR() error = %v, want the original gh pr list failure", err)
	}
	if host.useREST() {
		t.Fatal("an ordinary failure must not switch the host to REST")
	}
}

func TestGetChecksFallsBackToRESTWhenGraphQLIsRefused(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 9 --repo test/repo --json headRefOid --jq .headRefOid": {stderr: graphQLRefusal, code: 1},
		"gh api --method GET repos/test/repo/pulls/9":                      {stdout: `{"number":9,"head":{"ref":"feature","sha":"deadbeef"}}`},
		"gh api --method GET repos/test/repo/commits/deadbeef/check-runs -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":2,"check_runs":[
			{"id":11,"name":"build","status":"completed","conclusion":"failure","started_at":"2026-10-05T01:00:00Z","completed_at":"2026-10-05T01:05:00Z","details_url":"https://example.test/build","app":{"slug":"github-actions"}},
			{"id":12,"name":"review","status":"in_progress","conclusion":null,"details_url":"https://example.test/review","app":{"slug":"some-bot"}}
		]}]`},
		"gh api --method GET repos/test/repo/commits/deadbeef/status -f per_page=100 --paginate --slurp": {stdout: `[{"state":"success","statuses":[
			{"node_id":"SC_1","context":"deploy","state":"success","target_url":"https://example.test/deploy"}
		]}]`},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":0,"workflow_runs":[]}]`},
	}), func() bool { return true }, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "9", HeadSHA: "previous"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	byName := map[string]scm.Check{}
	for _, check := range checks {
		byName[check.Name] = check
	}
	if len(byName) != 3 {
		t.Fatalf("GetChecks() = %+v, want build, review, and deploy", checks)
	}
	if got := byName["build"]; got.Bucket != scm.CheckBucketFail || got.State != "FAILURE" || got.ProviderID != "github-check-run:11" || got.App != "github-actions" || got.StartedAt.IsZero() {
		t.Fatalf("build check = %+v, want a failed check run with its identity and app", got)
	}
	if got := byName["review"]; got.Bucket != scm.CheckBucketPending || got.App != "some-bot" {
		t.Fatalf("review check = %+v, want a pending check run from some-bot", got)
	}
	if got := byName["deploy"]; got.Kind != scm.CheckKindStatus || got.Bucket != scm.CheckBucketPass || got.ProviderID != "github-status:SC_1" {
		t.Fatalf("deploy status = %+v, want a passing commit status", got)
	}
}
