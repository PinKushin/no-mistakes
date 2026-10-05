package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// REST fallback for environments that refuse GitHub's GraphQL API.
//
// Most `gh pr` subcommands (list, create, edit, view, checks) and the commit
// check rollup query go through GraphQL. Some sandboxed environments - Claude
// Code cloud sessions among them - deny GraphQL with an HTTP 403 while the REST
// API keeps working with the same token. Without a fallback every PR and CI
// step there is skipped as "gh CLI is not authenticated".
//
// The fallback engages only on positive evidence that GraphQL itself was
// refused (graphQLRefused); any other failure keeps its existing error. Once a
// refusal is seen the Host stays on REST for the rest of its life, so later
// calls do not pay for a request that is known to fail. Results are mapped to
// the same shapes the GraphQL paths return, so callers cannot tell the paths
// apart.

// graphQLRefusedMarker is the message GitHub-fronting proxies return when they
// deny GraphQL while allowing REST.
const graphQLRefusedMarker = "GitHub GraphQL is not available"

// graphQLRefused reports whether gh output shows that GraphQL itself was
// refused rather than any ordinary failure (bad token, missing PR, network).
func graphQLRefused(output string) bool {
	if strings.Contains(output, graphQLRefusedMarker) {
		return true
	}
	lower := strings.ToLower(output)
	return strings.Contains(lower, "http 403") && strings.Contains(lower, "/graphql")
}

// commandOutput returns everything a failed command printed: the combined
// output when the caller captured it, plus stderr that exec.Cmd.Output keeps
// on the *exec.ExitError.
func commandOutput(out []byte, err error) string {
	text := string(out)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		text += "\n" + string(exitErr.Stderr)
	}
	return text
}

// useREST reports whether an earlier call already proved GraphQL is refused.
func (h *Host) useREST() bool {
	return h.graphQLRefused.Load()
}

// noteGraphQLRefusal records a refusal seen in a failed command's output and
// reports whether the caller should fall back to REST.
func (h *Host) noteGraphQLRefusal(out []byte, err error) bool {
	if err == nil || !graphQLRefused(commandOutput(out, err)) {
		return false
	}
	h.graphQLRefused.Store(true)
	return true
}

// restAPI runs one `gh api` REST call. body, when non-nil, is sent as the JSON
// request body on stdin. extra holds additional gh api flags (query fields,
// pagination) appended after the endpoint.
func (h *Host) restAPI(ctx context.Context, method, endpoint string, body any, extra ...string) ([]byte, error) {
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", method, endpoint)
	args = append(args, extra...)
	var stdin []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode gh api %s %s request: %w", method, endpoint, err)
		}
		stdin = encoded
		args = append(args, "--input", "-")
	}
	cmd := h.cmd(ctx, "gh", args...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api %s %s: %s: %w", method, endpoint, strings.TrimSpace(commandOutput(nil, err)), err)
	}
	return out, nil
}

// restRepo returns the owner/name slug REST endpoints are built from.
func (h *Host) restRepo() (string, error) {
	repo := h.repoSlug()
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("resolve GitHub repository for REST call: invalid repository %q", repo)
	}
	return repo, nil
}

// restPullNumber resolves a prSelector value (a number or a canonical PR URL)
// to the pull request number REST endpoints need.
func (h *Host) restPullNumber(selector string) (int, error) {
	selector = strings.TrimSpace(selector)
	if number, err := strconv.Atoi(selector); err == nil {
		if number <= 0 {
			return 0, errors.New("expected positive GitHub pull request number")
		}
		return number, nil
	}
	return parsePullRequestURL(selector, h.host, h.repoSlug())
}

// restPull is the subset of the REST pull request object these paths read.
type restPull struct {
	Number   int     `json:"number"`
	HTMLURL  string  `json:"html_url"`
	State    string  `json:"state"`
	MergedAt *string `json:"merged_at"`
	Title    *string `json:"title"`
	Body     *string `json:"body"`
	// Mergeable is null while GitHub is still computing it.
	Mergeable *bool `json:"mergeable"`
	Base      struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			Owner *struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repo"`
	} `json:"head"`
}

func (h *Host) restGetPull(ctx context.Context, selector string) (*restPull, error) {
	repo, err := h.restRepo()
	if err != nil {
		return nil, err
	}
	number, err := h.restPullNumber(selector)
	if err != nil {
		return nil, err
	}
	out, err := h.restAPI(ctx, "GET", fmt.Sprintf("repos/%s/pulls/%d", repo, number), nil)
	if err != nil {
		return nil, err
	}
	var pull restPull
	if err := json.Unmarshal(out, &pull); err != nil {
		return nil, fmt.Errorf("parse gh api pull request JSON: %w", err)
	}
	if pull.Number != number {
		return nil, fmt.Errorf("gh api pull request returned PR %d, want %d", pull.Number, number)
	}
	return &pull, nil
}

func (h *Host) restPatchPull(ctx context.Context, selector string, fields map[string]string) error {
	repo, err := h.restRepo()
	if err != nil {
		return err
	}
	number, err := h.restPullNumber(selector)
	if err != nil {
		return err
	}
	_, err = h.restAPI(ctx, "PATCH", fmt.Sprintf("repos/%s/pulls/%d", repo, number), fields)
	return err
}

func (h *Host) restFindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	repo, err := h.restRepo()
	if err != nil {
		return nil, err
	}
	owner := h.forkOwner
	if owner == "" {
		owner = repoOwner(repo)
	}
	extra := []string{"-f", "state=open", "-f", "head=" + owner + ":" + branch, "-f", "per_page=100"}
	if strings.TrimSpace(base) != "" {
		extra = append(extra, "-f", "base="+base)
	}
	out, err := h.restAPI(ctx, "GET", "repos/"+repo+"/pulls", nil, extra...)
	if err != nil {
		return nil, err
	}
	var pulls []restPull
	if err := json.Unmarshal(out, &pulls); err != nil {
		return nil, fmt.Errorf("parse gh api pull request list JSON: %w", err)
	}
	if pulls == nil {
		return nil, errors.New("parse gh api pull request list JSON: expected array")
	}
	for i, pull := range pulls {
		url := strings.TrimSpace(pull.HTMLURL)
		number, err := parsePullRequestURL(url, h.host, repo)
		if err != nil {
			return nil, fmt.Errorf("parse gh api pull request list JSON: entry %d invalid PR URL: %w", i, err)
		}
		if pull.Number != number {
			return nil, fmt.Errorf("parse gh api pull request list JSON: entry %d PR number %d does not match URL number %d", i, pull.Number, number)
		}
		if pull.Head.Ref != branch {
			continue
		}
		if pull.Head.Repo == nil || pull.Head.Repo.Owner == nil || !strings.EqualFold(pull.Head.Repo.Owner.Login, owner) {
			continue
		}
		return &scm.PR{
			Number:     strconv.Itoa(pull.Number),
			URL:        url,
			BaseBranch: strings.TrimSpace(pull.Base.Ref),
		}, nil
	}
	return nil, nil
}

func (h *Host) restCreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	repo, err := h.restRepo()
	if err != nil {
		return nil, err
	}
	request := map[string]any{
		"head":  h.headRef(branch),
		"base":  base,
		"title": content.Title,
		"body":  content.Body,
	}
	if h.draft {
		request["draft"] = true
	}
	out, err := h.restAPI(ctx, "POST", "repos/"+repo+"/pulls", request)
	if err != nil {
		return nil, err
	}
	var pull restPull
	if err := json.Unmarshal(out, &pull); err != nil {
		return nil, fmt.Errorf("parse gh api created pull request JSON: %w", err)
	}
	url := strings.TrimSpace(pull.HTMLURL)
	if url == "" || pull.Number <= 0 {
		return nil, errors.New("gh api create pull request returned no PR number or URL")
	}
	return &scm.PR{Number: strconv.Itoa(pull.Number), URL: url}, nil
}

func restPRState(pull *restPull) scm.PRState {
	if pull.MergedAt != nil && strings.TrimSpace(*pull.MergedAt) != "" {
		return scm.PRStateMerged
	}
	return normalizePRState(pull.State)
}

func restMergeableState(pull *restPull) scm.MergeableState {
	switch {
	case pull.Mergeable == nil:
		return scm.MergeablePending
	case *pull.Mergeable:
		return scm.MergeableOK
	default:
		return scm.MergeableConflict
	}
}

// restCommitChecks is the REST equivalent of getCommitChecks: the head
// commit's check runs (with their app slug) plus its latest commit statuses.
func (h *Host) restCommitChecks(ctx context.Context, headSHA string) ([]scm.Check, error) {
	repo, err := h.restRepo()
	if err != nil {
		return nil, err
	}
	sha := strings.TrimSpace(headSHA)
	out, err := h.restAPI(ctx, "GET", "repos/"+repo+"/commits/"+sha+"/check-runs", nil,
		"-f", "per_page=100", "--paginate", "--slurp")
	if err != nil {
		return nil, fmt.Errorf("gh api check runs for head commit: %w", err)
	}
	var runPages []struct {
		CheckRuns []struct {
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			Status      string `json:"status"`
			Conclusion  string `json:"conclusion"`
			StartedAt   string `json:"started_at"`
			CompletedAt string `json:"completed_at"`
			DetailsURL  string `json:"details_url"`
			App         *struct {
				Slug string `json:"slug"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(out, &runPages); err != nil {
		return nil, fmt.Errorf("parse check runs for head commit: %w", err)
	}
	var checks []scm.Check
	for _, page := range runPages {
		for _, run := range page.CheckRuns {
			check := scm.Check{Kind: scm.CheckKindRun, Name: strings.TrimSpace(run.Name)}
			if run.ID != 0 {
				check.ProviderID = fmt.Sprintf("github-check-run:%d", run.ID)
			}
			check.State = strings.ToUpper(strings.TrimSpace(run.Conclusion))
			if check.State == "" {
				check.State = strings.ToUpper(strings.TrimSpace(run.Status))
			}
			check.Bucket = normalizeCheckBucket("", run.Conclusion)
			if check.Bucket == "" {
				check.Bucket = normalizeCheckBucket("", run.Status)
			}
			check.Link = strings.TrimSpace(run.DetailsURL)
			if run.App != nil {
				check.App = strings.TrimSpace(run.App.Slug)
			}
			if parsed, parseErr := time.Parse(time.RFC3339, run.CompletedAt); parseErr == nil {
				check.CompletedAt = parsed
			}
			if parsed, parseErr := time.Parse(time.RFC3339, run.StartedAt); parseErr == nil {
				check.StartedAt = parsed
			}
			if check.Name == "" || check.Bucket == "" {
				return nil, errors.New("head commit check discovery returned an incomplete check run")
			}
			checks = append(checks, check)
		}
	}
	out, err = h.restAPI(ctx, "GET", "repos/"+repo+"/commits/"+sha+"/status", nil,
		"-f", "per_page=100", "--paginate", "--slurp")
	if err != nil {
		return nil, fmt.Errorf("gh api commit statuses for head commit: %w", err)
	}
	var statusPages []struct {
		Statuses []struct {
			NodeID    string `json:"node_id"`
			Context   string `json:"context"`
			State     string `json:"state"`
			TargetURL string `json:"target_url"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(out, &statusPages); err != nil {
		return nil, fmt.Errorf("parse commit statuses for head commit: %w", err)
	}
	for _, page := range statusPages {
		for _, status := range page.Statuses {
			check := scm.Check{
				Kind:   scm.CheckKindStatus,
				Name:   strings.TrimSpace(status.Context),
				State:  strings.ToUpper(strings.TrimSpace(status.State)),
				Bucket: normalizeCheckBucket("", status.State),
				Link:   strings.TrimSpace(status.TargetURL),
			}
			if status.NodeID != "" {
				check.ProviderID = "github-status:" + status.NodeID
			}
			if check.Name == "" || check.Bucket == "" {
				return nil, errors.New("head commit check discovery returned an incomplete commit status")
			}
			checks = append(checks, check)
		}
	}
	return checks, nil
}

// restPRChecks replaces `gh pr checks` (GraphQL): it reads the PR's current
// head and reports that commit's checks through the same rollup, workflow-run
// union, and rerun collapse the head-SHA path of GetChecks applies.
func (h *Host) restPRChecks(ctx context.Context, selector string) ([]scm.Check, error) {
	pull, err := h.restGetPull(ctx, selector)
	if err != nil {
		return nil, err
	}
	headSHA := strings.TrimSpace(pull.Head.SHA)
	if headSHA == "" {
		return nil, errors.New("gh api pull request returned an empty head commit")
	}
	checks, err := h.restCommitChecks(ctx, headSHA)
	if err != nil {
		return nil, err
	}
	runs, err := h.getWorkflowRunChecks(ctx, headSHA)
	if err != nil {
		return nil, err
	}
	checks = h.appendUnrepresentedWorkflowRuns(checks, runs)
	return h.collapseLatestByName(checks), nil
}

// restAuthWorks reports whether the configured token answers a REST call. It
// is consulted only after `gh auth status` failed, which also happens when the
// environment refuses the GraphQL request that command makes.
func (h *Host) restAuthWorks(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	_, err := h.restAPI(ctx, "GET", "user", nil)
	return err == nil
}
