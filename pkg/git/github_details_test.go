package v1alpha1

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	githubClient "github.com/google/go-github/v42/github"
)

// The fixture is a verbatim item from GitHub's "list pull requests" response
// (the same endpoint Poll() calls), so it carries the full head/base repo
// objects, body, user, _links, ... exactly as production receives them.
const fixturePath = "testdata/github_pr_list_item.json"

const (
	fixtureSHA    = "64e71a66f70d117689ce8c013033c18c641d7dd5"
	fixtureRef    = "zach/broaden-test-ci"
	fixtureNumber = 3
)

func loadFixturePR(t *testing.T) *githubClient.PullRequest {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var pr githubClient.PullRequest
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return &pr
}

// fullLabel builds a label the way the GitHub API returns it (id, node_id, url,
// color, description, default), so the test proves the extra label fields are
// dropped and only the name is kept.
func fullLabel(id int64, name string) *githubClient.Label {
	nodeID := "LA_kwDO" + name
	url := "https://api.github.com/repos/civitai/pullrequest-operator/labels/" + name
	color := "ededed"
	desc := "label " + name + " description"
	def := false
	return &githubClient.Label{ID: &id, NodeID: &nodeID, URL: &url, Name: &name, Color: &color, Description: &desc, Default: &def}
}

// resolvePath resolves a simple "$.a.b" JSONPath (dotted object keys only, the
// form every PullRequest PipelineTrigger uses) against a JSON document. ok is
// false when any segment is missing.
func resolvePath(t *testing.T, doc, path string) (interface{}, bool) {
	t.Helper()
	var cur interface{}
	if err := json.Unmarshal([]byte(doc), &cur); err != nil {
		t.Fatalf("Details is not valid JSON: %v (%q)", err, doc)
	}
	for _, seg := range strings.Split(strings.TrimPrefix(path, "$."), ".") {
		m, isObj := cur.(map[string]interface{})
		if !isObj {
			return nil, false
		}
		v, found := m[seg]
		if !found {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// TestDetailsConsumerJSONPaths pins the values the in-cluster PipelineTriggers
// read from Details ($.number, $.head.sha, $.author_association) plus the other
// kept paths, for a realistic PR. Each path must resolve, to the fixture value.
func TestDetailsConsumerJSONPaths(t *testing.T) {
	b, err := branchFromPR(loadFixturePR(t))
	if err != nil {
		t.Fatalf("branchFromPR: %v", err)
	}
	want := map[string]interface{}{
		"$.number":             float64(fixtureNumber),
		"$.head.sha":           fixtureSHA,
		"$.author_association": "MEMBER",
		"$.head.ref":           fixtureRef,
		"$.head.label":         "civitai:" + fixtureRef,
		"$.base.ref":           "feat/label-retrigger",
		"$.base.sha":           "109afc43b01774365dff6e0fdfea3a229895f37f",
		"$.title":              "ci: broaden test workflow to run the full go test ./... suite",
		"$.user.login":         "ZacxDev",
		"$.state":              "closed",
		"$.draft":              false,
		"$.html_url":           "https://github.com/civitai/pullrequest-operator/pull/3",
		"$.statuses_url":       "https://api.github.com/repos/civitai/pullrequest-operator/statuses/" + fixtureSHA,
	}
	for path, wantV := range want {
		got, ok := resolvePath(t, b.Details, path)
		if !ok {
			t.Errorf("%s does not resolve in Details %s", path, b.Details)
			continue
		}
		if got != wantV {
			t.Errorf("%s = %#v, want %#v", path, got, wantV)
		}
	}
}

// TestDetailsIsTrimmedToContract: Details holds EXACTLY the kept fields (pinned
// as the whole string, so adding or dropping any field fails) and is small.
// On the pre-trim code Details was the full ~20 KB PR object.
func TestDetailsIsTrimmedToContract(t *testing.T) {
	pr := loadFixturePR(t)
	full, _ := json.Marshal(pr)

	b, err := branchFromPR(pr)
	if err != nil {
		t.Fatalf("branchFromPR: %v", err)
	}
	// Size first, so a regression to the full object reports as a size failure.
	if len(b.Details) >= 1024 {
		t.Fatalf("Details is %d bytes, want < 1024 (full PR marshals to %d)", len(b.Details), len(full))
	}
	const wantDetails = `{"number":3,"state":"closed","title":"ci: broaden test workflow to run the full go test ./... suite","draft":false,"author_association":"MEMBER","html_url":"https://github.com/civitai/pullrequest-operator/pull/3","statuses_url":"https://api.github.com/repos/civitai/pullrequest-operator/statuses/64e71a66f70d117689ce8c013033c18c641d7dd5","user":{"login":"ZacxDev"},"head":{"label":"civitai:zach/broaden-test-ci","ref":"zach/broaden-test-ci","sha":"64e71a66f70d117689ce8c013033c18c641d7dd5"},"base":{"label":"civitai:feat/label-retrigger","ref":"feat/label-retrigger","sha":"109afc43b01774365dff6e0fdfea3a229895f37f"}}`
	if b.Details != wantDetails {
		t.Fatalf("Details mismatch\n got: %s\nwant: %s", b.Details, wantDetails)
	}
	t.Logf("Details: %d bytes (full PR object: %d bytes)", len(b.Details), len(full))

}

// TestDetailsLabeledPR: a labeled realistic PR keeps label NAMES only (in
// GitHub's order), still fits the size budget, and the full Details string is
// pinned.
func TestDetailsLabeledPR(t *testing.T) {
	pr := loadFixturePR(t)
	pr.Labels = []*githubClient.Label{fullLabel(101, "preview"), fullLabel(102, "preview-db/prod")}

	b, err := branchFromPR(pr)
	if err != nil {
		t.Fatalf("branchFromPR: %v", err)
	}
	const wantDetails = `{"number":3,"state":"closed","title":"ci: broaden test workflow to run the full go test ./... suite","draft":false,"author_association":"MEMBER","html_url":"https://github.com/civitai/pullrequest-operator/pull/3","statuses_url":"https://api.github.com/repos/civitai/pullrequest-operator/statuses/64e71a66f70d117689ce8c013033c18c641d7dd5","user":{"login":"ZacxDev"},"labels":[{"name":"preview"},{"name":"preview-db/prod"}],"head":{"label":"civitai:zach/broaden-test-ci","ref":"zach/broaden-test-ci","sha":"64e71a66f70d117689ce8c013033c18c641d7dd5"},"base":{"label":"civitai:feat/label-retrigger","ref":"feat/label-retrigger","sha":"109afc43b01774365dff6e0fdfea3a229895f37f"}}`
	if b.Details != wantDetails {
		t.Fatalf("labeled Details mismatch\n got: %s\nwant: %s", b.Details, wantDetails)
	}
	if len(b.Details) >= 1024 {
		t.Fatalf("labeled Details is %d bytes, want < 1024", len(b.Details))
	}
}

// TestCommitDiscriminatorUnchangedByTrim pins Branch.Name/Commit to literal
// values computed on the pre-trim code for the same fixture. Trimming Details
// must not change the discriminator, or every open PR would re-fire a
// PipelineRun on rollout. (Invariant guard: this also passes on the pre-trim
// code by design.)
func TestCommitDiscriminatorUnchangedByTrim(t *testing.T) {
	unlabeled, err := branchFromPR(loadFixturePR(t))
	if err != nil {
		t.Fatalf("branchFromPR: %v", err)
	}
	if unlabeled.Name != fixtureRef {
		t.Fatalf("Name = %q, want %q", unlabeled.Name, fixtureRef)
	}
	if unlabeled.Commit != fixtureSHA {
		t.Fatalf("unlabeled Commit = %q, want bare SHA %q", unlabeled.Commit, fixtureSHA)
	}

	pr := loadFixturePR(t)
	// Deliberately out of order: the suffix is computed on the sorted names.
	pr.Labels = []*githubClient.Label{fullLabel(102, "preview-db/prod"), fullLabel(101, "preview")}
	labeled, err := branchFromPR(pr)
	if err != nil {
		t.Fatalf("branchFromPR: %v", err)
	}
	// sha256("preview,preview-db/prod")[:12] == da756549e030
	const wantLabeled = fixtureSHA + "-da756549e030"
	if labeled.Commit != wantLabeled {
		t.Fatalf("labeled Commit = %q, want %q", labeled.Commit, wantLabeled)
	}
}
