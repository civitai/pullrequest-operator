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

	opts := githubClient.PullRequestListOptions{Base: branch}

	var prList []*githubClient.PullRequest
	var prResponse *githubClient.Response
	prList, prResponse, err := client.PullRequests.List(ctx, githubPoller.Owner, githubPoller.Repository, &opts)
	eTagUnparsed := prResponse.Header.Get("ETag")
	eTag := ""
	if strings.Contains(eTagUnparsed, "W/") {
		eTag = strings.Split(prResponse.Header.Get("ETag"), "/")[1]
	} else {
		eTag = prResponse.Header.Get("ETag")
	}
	if prResponse.StatusCode == http.StatusNotModified {
		return branches, eTag, nil
	}
	if err != nil {
		fmt.Println(prResponse)
		fmt.Println(err)
		return branches, "", err
	}

	sourceBranches := make([]pullrequestv1alpha1.Branch, len(prList))

	for i := 0; i < len(prList); i++ {
		var tempBranch pullrequestv1alpha1.Branch
		tempBranch.Name = prList[i].GetHead().GetRef()
		tempBranch.Commit = prList[i].GetHead().GetSHA()
		// civitai fork: fold the PR label set into the Commit discriminator so a
		// label-only change retriggers the pipeline. Upstream keys event-detection
		// and PipelineRun de-dup solely on Commit, and Branch.Equals ignores
		// Details (where labels live), so without this a label add/remove produces
		// no status diff and nothing fires. Suffix is appended ONLY when labels are
		// present, so unlabeled PRs keep the bare SHA (backward compatible). The
		// real SHA still flows untouched through Details ($.head.sha), which is what
		// our PipelineTriggers read for PR_SHA.
		if len(prList[i].Labels) > 0 {
			names := make([]string, 0, len(prList[i].Labels))
			for _, l := range prList[i].Labels {
				names = append(names, l.GetName())
			}
			sort.Strings(names) // order-independent: GitHub label reordering must not refire
			sum := sha256.Sum256([]byte(strings.Join(names, ",")))
			tempBranch.Commit = tempBranch.Commit + "-" + hex.EncodeToString(sum[:])[:12]
		}
		pr, err := json.Marshal(prList[i])
		if err != nil {
			//fmt.Println(err)
			return branches, "", err
		}
		tempBranch.Details = string(pr)
		sourceBranches[i] = tempBranch
	}
	branches.Branches = sourceBranches

	return branches, eTag, nil
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
