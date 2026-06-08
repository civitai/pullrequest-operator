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

// branchFromPR maps a GitHub pull request to a Branch, folding the label set
// into the Commit discriminator. Unlabeled PRs keep the bare head SHA (backward
// compatible); the untouched SHA always remains available in Details.
func branchFromPR(pr *githubClient.PullRequest) (pullrequestv1alpha1.Branch, error) {
	var b pullrequestv1alpha1.Branch
	b.Name = pr.GetHead().GetRef()
	b.Commit = pr.GetHead().GetSHA() + labelCommitSuffix(pr.Labels)
	details, err := json.Marshal(pr)
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
