package v1alpha1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	githubClient "github.com/google/go-github/v42/github"
)

func labels(names ...string) []*githubClient.Label {
	out := make([]*githubClient.Label, 0, len(names))
	for _, n := range names {
		n := n
		out = append(out, &githubClient.Label{Name: &n})
	}
	return out
}

func TestLabelCommitSuffix(t *testing.T) {
	// No labels => empty suffix (unlabeled PRs keep the bare SHA; backward compatible).
	if got := labelCommitSuffix(nil); got != "" {
		t.Fatalf("nil labels: want empty suffix, got %q", got)
	}
	if got := labelCommitSuffix(labels()); got != "" {
		t.Fatalf("empty labels: want empty suffix, got %q", got)
	}

	// A label set produces a "-<12 hex>" suffix.
	s := labelCommitSuffix(labels("preview"))
	if !strings.HasPrefix(s, "-") || len(s) != 13 {
		t.Fatalf("single label: want -<12hex>, got %q (len %d)", s, len(s))
	}

	// Deterministic: same set => same suffix.
	if labelCommitSuffix(labels("preview")) != s {
		t.Fatal("suffix is not deterministic for the same label set")
	}

	// Order-independent: re-ordering labels must NOT change the suffix
	// (otherwise GitHub label reordering would spuriously refire).
	if labelCommitSuffix(labels("a", "b")) != labelCommitSuffix(labels("b", "a")) {
		t.Fatal("suffix changed when only label order changed")
	}

	// Distinct sets => distinct suffixes (a real add/remove must refire).
	if labelCommitSuffix(labels("preview")) == labelCommitSuffix(labels("preview", "preview-db/prod")) {
		t.Fatal("different label sets produced the same suffix")
	}
}

func newPR(ref, sha string, lbls []*githubClient.Label) *githubClient.PullRequest {
	return &githubClient.PullRequest{
		Head:   &githubClient.PullRequestBranch{Ref: &ref, SHA: &sha},
		Labels: lbls,
	}
}

func TestBranchFromPR(t *testing.T) {
	const sha = "ccb126e0000000000000000000000000000000aa"

	// Unlabeled PR keeps the bare SHA.
	b, err := branchFromPR(newPR("feat/x", sha, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.Name != "feat/x" {
		t.Fatalf("Name: want feat/x, got %q", b.Name)
	}
	if b.Commit != sha {
		t.Fatalf("unlabeled Commit: want bare SHA %q, got %q", sha, b.Commit)
	}
	if !strings.Contains(b.Details, `"ref":"feat/x"`) {
		t.Fatalf("Details should carry the marshaled PR, got %q", b.Details)
	}

	// Labeled PR gets the composite, and the real SHA is still recoverable in Details.
	bl, err := branchFromPR(newPR("feat/x", sha, labels("preview")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bl.Commit == sha || !strings.HasPrefix(bl.Commit, sha+"-") {
		t.Fatalf("labeled Commit: want %q + suffix, got %q", sha, bl.Commit)
	}
	if !strings.Contains(bl.Details, `"name":"preview"`) {
		t.Fatalf("labeled Details should contain the label, got %q", bl.Details)
	}
}

// TestPollPaginatesAllOpenPRs stands up a fake GitHub Enterprise API that serves
// two pages of open PRs and asserts Poll() collects every PR across all pages
// (upstream only read page 1) and folds labels into the commit discriminator.
func TestPollPaginatesAllOpenPRs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/pulls") {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `W/"etag-test"`)
		switch r.URL.Query().Get("page") {
		case "", "1":
			// rel="next" → go-github parses NextPage=2 from the page query param.
			w.Header().Set("Link", `<http://example/repos/o/r/pulls?page=2>; rel="next"`)
			w.Write([]byte(`[
				{"head":{"ref":"branch-a","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"labels":[{"name":"preview"}]},
				{"head":{"ref":"branch-b","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"labels":[]}
			]`))
		case "2":
			w.Write([]byte(`[
				{"head":{"ref":"branch-c","sha":"cccccccccccccccccccccccccccccccccccccccc"},"labels":[{"name":"preview-db/prod"}]}
			]`))
		default:
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	poller := NewGithubPoller(srv.URL, "", false, "o", "r")
	branches, _, err := poller.Poll("main", "")
	if err != nil {
		t.Fatalf("Poll error: %v", err)
	}

	got := map[string]string{}
	for _, b := range branches.Branches {
		got[b.Name] = b.Commit
	}
	if len(got) != 3 {
		t.Fatalf("want 3 PRs across 2 pages, got %d: %v", len(got), got)
	}
	if _, ok := got["branch-c"]; !ok {
		t.Fatal("page-2 PR (branch-c) missing → pagination not walked")
	}
	// unlabeled keeps bare SHA; labeled gets composite
	if got["branch-b"] != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("branch-b should keep bare SHA, got %q", got["branch-b"])
	}
	if !strings.HasPrefix(got["branch-a"], "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-") {
		t.Fatalf("branch-a should be composite, got %q", got["branch-a"])
	}
	if !strings.HasPrefix(got["branch-c"], "cccccccccccccccccccccccccccccccccccccccc-") {
		t.Fatalf("branch-c should be composite, got %q", got["branch-c"])
	}
}

// TestPollNotModified304 covers the conditional-request short-circuit: when the
// GitHub API responds 304 Not Modified on page 1, Poll must return the parsed
// ETag, an EMPTY branch set, and no error — never a partial/empty status write.
// This is the provider-layer churn guard (its Reconcile-layer sibling early-
// returns when the ETag is unchanged). Without it, an unchanged repo would still
// be re-diffed and re-written each interval.
func TestPollNotModified304(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/pulls") {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		hits++
		w.Header().Set("ETag", `W/"abc123"`)
		// Respond 304 regardless of If-None-Match so the test is deterministic.
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	poller := NewGithubPoller(srv.URL, "", false, "o", "r")
	branches, etag, err := poller.Poll("main", `"abc123"`)
	if err != nil {
		t.Fatalf("Poll on 304 should not error, got %v", err)
	}
	if etag != `"abc123"` {
		t.Fatalf("Poll 304 etag = %q, want %q (parsed from weak W/ tag)", etag, `"abc123"`)
	}
	if len(branches.Branches) != 0 {
		t.Fatalf("Poll 304 should return no branches, got %v", branches.Branches)
	}
	if hits != 1 {
		t.Fatalf("expected exactly one request (no pagination past a 304), got %d", hits)
	}
}

// TestBranchFromPRLabelRetriggerContract is the explicit fork-behavior guard:
// adding a label to a PR must change the Commit discriminator (so the change
// propagates as a retrigger), while re-polling the identical PR must keep it
// byte-stable (so nothing spuriously refires).
func TestBranchFromPRLabelRetriggerContract(t *testing.T) {
	const sha = "abcabcabcabcabcabcabcabcabcabcabcabcabca"

	unlabeled, _ := branchFromPR(newPR("feat/x", sha, nil))
	labeled, _ := branchFromPR(newPR("feat/x", sha, labels("preview")))
	if unlabeled.Commit == labeled.Commit {
		t.Fatalf("adding a label must change the commit discriminator: both %q", unlabeled.Commit)
	}

	// Same inputs -> byte-identical discriminator (no spurious retrigger).
	labeledAgain, _ := branchFromPR(newPR("feat/x", sha, labels("preview")))
	if labeled.Commit != labeledAgain.Commit {
		t.Fatalf("identical PR produced different discriminators: %q vs %q", labeled.Commit, labeledAgain.Commit)
	}
}
