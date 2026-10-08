package v1alpha1

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	githubClient "github.com/google/go-github/v42/github"
	pullrequestv1alpha1 "github.com/jquad-group/pullrequest-operator/api/v1alpha1"
	"golang.org/x/oauth2"
)

type GithubPoller struct {
	Endpoint           string
	AccessToken        string
	InsecureSkipVerify bool
	Owner              string
	Repository         string
}

func NewGithubPoller(endpoint string, accessToken string, insecureSkipVerify bool, owner string, repository string) *GithubPoller {
	return &GithubPoller{
		Endpoint:           endpoint,
		AccessToken:        accessToken,
		InsecureSkipVerify: insecureSkipVerify,
		Owner:              owner,
		Repository:         repository,
	}
}

func (githubPoller GithubPoller) Poll(branch string, etag string) (pullrequestv1alpha1.Branches, string, error) {
	ctx := context.Background()
	/*
		transportHeaders := transportHeaders{
			eTag: etag,
		}
	*/

	// check if we accept untrusted certificates
	var httpTransport *http.Transport
	if githubPoller.InsecureSkipVerify {
		httpTransport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}

	} else {
		httpTransport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
		}
	}

	httpClient := &http.Client{Transport: &transportHeaders{eTag: etag, transport: httpTransport}}

	var tc *http.Client
	// check if we provided an access token
	if len(githubPoller.AccessToken) > 0 {
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: githubPoller.AccessToken},
		)
		ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)
		tc = oauth2.NewClient(ctx, ts)
	} else {
		tc = nil
	}

	var branches pullrequestv1alpha1.Branches
	var client *githubClient.Client
	var errClient error
	// check if the base url is github.com or an enterprise github server
	if !strings.HasPrefix(githubPoller.Endpoint, "https://github.com/") {
		gheEndpoint, err := url.Parse(githubPoller.Endpoint)
		if err != nil {
			fmt.Println(err)
		}
		client, errClient = githubClient.NewEnterpriseClient(gheEndpoint.Scheme+"://"+gheEndpoint.Host, gheEndpoint.Scheme+"://"+gheEndpoint.Host, tc)
		if errClient != nil {
			fmt.Println(errClient)
			return branches, "", errClient
		}
	} else {
		client = githubClient.NewClient(tc)
	}

	opts := githubClient.PullRequestListOptions{
		Base:        branch,
		ListOptions: githubClient.ListOptions{PerPage: 100},
	}

	// Paginate through ALL open PRs targeting the base branch. Upstream issued a
	// single List() (GitHub default per_page=30), silently ignoring every PR
	// beyond the first page — those PRs got no previews/checks at all regardless
	// of commits or labels. Walk every page so coverage is complete.
	var prList []*githubClient.PullRequest
	eTag := ""
	for page := 1; ; {
		pagePRs, prResponse, listErr := client.PullRequests.List(ctx, githubPoller.Owner, githubPoller.Repository, &opts)
		if prResponse == nil {
			return branches, "", listErr
		}
		if page == 1 {
			eTagUnparsed := prResponse.Header.Get("ETag")
			if strings.Contains(eTagUnparsed, "W/") {
				eTag = strings.Split(eTagUnparsed, "/")[1]
			} else {
				eTag = eTagUnparsed
			}
			// Conditional-request short-circuit: page 1 unchanged since last poll.
			if prResponse.StatusCode == http.StatusNotModified {
				return branches, eTag, nil
			}
		}
		if listErr != nil {
			fmt.Println(prResponse)
			fmt.Println(listErr)
			return branches, "", listErr
		}
		prList = append(prList, pagePRs...)
		if prResponse.NextPage == 0 {
			break
		}
		page = prResponse.NextPage
		opts.Page = prResponse.NextPage
	}

	sourceBranches := make([]pullrequestv1alpha1.Branch, 0, len(prList))
	for _, pr := range prList {
		tempBranch, marshalErr := branchFromPR(pr)
		if marshalErr != nil {
			return branches, "", marshalErr
		}
		sourceBranches = append(sourceBranches, tempBranch)
	}
	branches.Branches = sourceBranches

	return branches, eTag, nil
}

// labelCommitSuffix returns "-<12 hex>" derived from a hash of the sorted PR
// label names, or "" when the PR has no labels. It is deterministic and
// order-independent, so re-ordering labels on GitHub does not refire, but any
// add/remove changes the suffix. Folding this into Branch.Commit is what makes
// a label-only change retrigger the pipeline: Branch.Equals compares Commit but
// not Details (where the labels otherwise live), so without it a label edit
// produces no status diff and nothing downstream fires.
func labelCommitSuffix(labels []*githubClient.Label) string {
	if len(labels) == 0 {
		return ""
	}
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.GetName())
	}
	sort.Strings(names)
	sum := sha256.Sum256([]byte(strings.Join(names, ",")))
	return "-" + hex.EncodeToString(sum[:])[:12]
}

// prDetails is the CONTRACT for what Branch.Details holds: a trimmed subset of
// the GitHub pull-request object, NOT the full object.
//
// Why trimmed: every Branch (one per open PR) is stored in the PullRequest CR's
// status, and the full GitHub PR JSON is ~20 KB per PR (the head/base repo
// objects alone are ~6 KB each, plus body, user, _links, ...). With ~76 open
// PRs the CR approached the etcd object size limit (~1.5 MiB), status writes
// started failing, and downstream PipelineRuns stopped being created.
//
// Shape: every kept field uses the SAME JSON key, nesting and omitempty
// semantics as go-github's PullRequest marshaling (the pointer fields are copied
// straight from it), so an existing JSONPath such as "$.head.sha" resolves to
// exactly the value it did before. Consumers (pipeline-trigger-operator's
// PullRequest source) evaluate arbitrary PipelineTrigger param JSONPaths against
// this string; a path naming a field NOT listed here evaluates to null there,
// its param check fails ("Expression ... evaluates to null") and that PR gets an
// Error condition and NO PipelineRun. So ADD the field here before using it in
// a PipelineTrigger.
//
// Kept fields and who reads them:
//   - number             PipelineTrigger params PR_NUMBER=$.number (all PR triggers)
//   - head.sha           PipelineTrigger params PR_SHA / GIT_REVISION=$.head.sha; the
//     real SHA when Branch.Commit carries the label suffix
//   - author_association PipelineTrigger param AUTHOR_ASSOCIATION=$.author_association
//     (the PR-preview triggers)
//   - head.ref           Branch.Name source; upstream pipeline-trigger-operator
//     sample trigger (ci/pipelinetrigger-pullrequest.yaml) reads $.head.ref
//   - head.label         upstream sample (examples/create-pipeline-on-pr) reads $.head.label
//   - statuses_url       upstream sample (ci/pipelinetrigger-pullrequest.yaml) reads it
//   - title              documented in the pipeline-trigger-operator README ($.title)
//   - labels[].name      the PR's label set (the same names labelCommitSuffix folds
//     into Branch.Commit, which is computed from the PR, not from Details)
//   - base.ref/base.sha, user.login, state, draft, html_url
//     small identifying fields kept so a trigger can address the PR's target,
//     author and URL without the full object.
//
// Deliberately dropped: body, head.repo/base.repo, user (beyond login),
// _links, assignees, reviewers, milestone and every *_url except the two above.
type prDetails struct {
	Number            *int             `json:"number,omitempty"`
	State             *string          `json:"state,omitempty"`
	Title             *string          `json:"title,omitempty"`
	Draft             *bool            `json:"draft,omitempty"`
	AuthorAssociation *string          `json:"author_association,omitempty"`
	HTMLURL           *string          `json:"html_url,omitempty"`
	StatusesURL       *string          `json:"statuses_url,omitempty"`
	User              *prUserDetails   `json:"user,omitempty"`
	Labels            []prLabelDetails `json:"labels,omitempty"`
	Head              *prBranchDetails `json:"head,omitempty"`
	Base              *prBranchDetails `json:"base,omitempty"`
}

type prUserDetails struct {
	Login *string `json:"login,omitempty"`
}

type prLabelDetails struct {
	Name *string `json:"name,omitempty"`
}

type prBranchDetails struct {
	Label *string `json:"label,omitempty"`
	Ref   *string `json:"ref,omitempty"`
	SHA   *string `json:"sha,omitempty"`
}

func trimBranch(b *githubClient.PullRequestBranch) *prBranchDetails {
	if b == nil {
		return nil
	}
	return &prBranchDetails{Label: b.Label, Ref: b.Ref, SHA: b.SHA}
}

// trimPR projects a GitHub pull request onto the prDetails contract.
func trimPR(pr *githubClient.PullRequest) prDetails {
	d := prDetails{
		Number:            pr.Number,
		State:             pr.State,
		Title:             pr.Title,
		Draft:             pr.Draft,
		AuthorAssociation: pr.AuthorAssociation,
		HTMLURL:           pr.HTMLURL,
		StatusesURL:       pr.StatusesURL,
		Head:              trimBranch(pr.Head),
		Base:              trimBranch(pr.Base),
	}
	if pr.User != nil {
		d.User = &prUserDetails{Login: pr.User.Login}
	}
	for _, l := range pr.Labels {
		if l == nil {
			continue
		}
		d.Labels = append(d.Labels, prLabelDetails{Name: l.Name})
	}
	return d
}

// branchFromPR maps a GitHub pull request to a Branch, folding the label set
// into the Commit discriminator. Unlabeled PRs keep the bare head SHA (backward
// compatible); the untouched SHA always remains available in Details
// ($.head.sha). Details holds the trimmed prDetails projection, not the full PR.
func branchFromPR(pr *githubClient.PullRequest) (pullrequestv1alpha1.Branch, error) {
	var b pullrequestv1alpha1.Branch
	b.Name = pr.GetHead().GetRef()
	b.Commit = pr.GetHead().GetSHA() + labelCommitSuffix(pr.Labels)
	details, err := json.Marshal(trimPR(pr))
	if err != nil {
		return b, err
	}
	b.Details = string(details)
	return b, nil
}

type transportHeaders struct {
	eTag      string
	transport *http.Transport
}

func (t *transportHeaders) RoundTrip(req *http.Request) (*http.Response, error) {

	if t.eTag != "" {
		req.Header.Set("If-None-Match", t.eTag)
	}

	return http.DefaultTransport.RoundTrip(req)
}
