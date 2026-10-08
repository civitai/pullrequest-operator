package controllers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	pipelinev1alpha1 "github.com/jquad-group/pullrequest-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// bodyCaptureClient records the exact bytes Reconcile's status apply-patch would
// send to the API server (client.Patch.Data is what controller-runtime puts on
// the wire).
type bodyCaptureClient struct {
	client.Client
	bodies *[][]byte
}

func (c bodyCaptureClient) Status() client.StatusWriter {
	return bodyCaptureStatusWriter{StatusWriter: c.Client.Status(), bodies: c.bodies}
}

type bodyCaptureStatusWriter struct {
	client.StatusWriter
	bodies *[][]byte
}

func (w bodyCaptureStatusWriter) Patch(_ context.Context, obj client.Object, p client.Patch, _ ...client.PatchOption) error {
	b, err := p.Data(obj)
	if err != nil {
		return err
	}
	*w.bodies = append(*w.bodies, b)
	return nil
}

// reconcileStatusBody runs one Reconcile with the CR currently holding `stored`
// and the poll returning `polled`, and returns the decoded status apply body.
func reconcileStatusBody(t *testing.T, stored []pipelinev1alpha1.Branch, polled pipelinev1alpha1.Branches) map[string]interface{} {
	t.Helper()
	pr := basePR()
	pr.Status.SourceBranches.Branches = stored
	pr.Status.ETag = "etag-old"
	r, cl, _ := reconcilerFor(t, pr, fakePoller{branches: polled, etag: "etag-new"})
	var bodies [][]byte
	r.Client = bodyCaptureClient{Client: cl, bodies: &bodies}
	if _, err := r.Reconcile(context.Background(), reqFor(pr)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("expected exactly one status apply-patch, got %d", len(bodies))
	}
	var body map[string]interface{}
	if err := json.Unmarshal(bodies[0], &body); err != nil {
		t.Fatalf("decode body %s: %v", bodies[0], err)
	}
	status, ok := body["status"].(map[string]interface{})
	if !ok {
		t.Fatalf("body has no status object: %s", bodies[0])
	}
	return status
}

// TestReconcileEmptyPollWritesExplicitEmptyBranches: when the poll matches no
// open PRs, the status apply body must carry sourceBranches as an OBJECT whose
// branches is an explicit empty ARRAY. Sending `"sourceBranches":{}` (what the
// omitempty tag produced) makes the API server's server-side apply store
// sourceBranches as null for a manager that previously owned .branches, which
// the CRD rejects — leaving the CR stuck on its last non-empty list.
// Both shapes a poller can return for "no PRs" (nil and empty slice) are covered.
func TestReconcileEmptyPollWritesExplicitEmptyBranches(t *testing.T) {
	stored := []pipelinev1alpha1.Branch{{Name: "stale/branch", Commit: "sha-stale"}}
	for name, polled := range map[string]pipelinev1alpha1.Branches{
		"nil slice":   {},
		"empty slice": {Branches: []pipelinev1alpha1.Branch{}},
	} {
		t.Run(name, func(t *testing.T) {
			status := reconcileStatusBody(t, stored, polled)
			sb, ok := status["sourceBranches"].(map[string]interface{})
			if !ok {
				t.Fatalf("status.sourceBranches must be an object, got %#v", status["sourceBranches"])
			}
			branches, present := sb["branches"]
			if !present {
				t.Fatalf("status.sourceBranches.branches must be present (explicit []), got %#v", sb)
			}
			arr, ok := branches.([]interface{})
			if !ok || arr == nil {
				t.Fatalf("status.sourceBranches.branches must be a JSON array, got %#v", branches)
			}
			if len(arr) != 0 {
				t.Fatalf("status.sourceBranches.branches must be empty, got %#v", arr)
			}
			if status["etag"] != "etag-new" {
				t.Fatalf("etag must still be written, got %#v", status["etag"])
			}
		})
	}
}

// TestReconcileNonEmptyPollBodyUnchanged: the explicit-empty handling must not
// alter a normal write — the polled branches are sent verbatim.
func TestReconcileNonEmptyPollBodyUnchanged(t *testing.T) {
	polled := pipelinev1alpha1.Branches{Branches: []pipelinev1alpha1.Branch{
		{Name: "feat/a", Commit: "sha-a", Details: `{"number":1}`},
		{Name: "feat/b", SHA: "sha-b2", Commit: "sha-b"},
	}}
	status := reconcileStatusBody(t, nil, polled)
	want := map[string]interface{}{"branches": []interface{}{
		map[string]interface{}{"name": "feat/a", "commit": "sha-a", "details": `{"number":1}`},
		map[string]interface{}{"name": "feat/b", "sha": "sha-b2", "commit": "sha-b"},
	}}
	if !reflect.DeepEqual(status["sourceBranches"], want) {
		t.Fatalf("sourceBranches = %#v, want %#v", status["sourceBranches"], want)
	}
}

// TestEmptyStatusAppliesAgainstRealAPIServer reproduces the production failure
// end-to-end against a real kube-apiserver + the CRD in config/crd/bases:
// server-side apply of 1 branch, then of the empty-poll status Reconcile builds.
// The old `"sourceBranches":{}` body is rejected with
// `status.sourceBranches: Invalid value: "null"`; the fixed body must be
// accepted and stored as {"branches": []}.
//
// The null is produced by the API server's apply merge, not by our JSON, so no
// client-side check can see it; this test needs envtest binaries. It SKIPS (and
// says so) without KUBEBUILDER_ASSETS — CI's plain `go test` does not set it.
// Run locally with:
//
//	KUBEBUILDER_ASSETS=$(setup-envtest use 1.33.0 -p path) go test ./controllers -run TestEmptyStatusAppliesAgainstRealAPIServer -v
func TestEmptyStatusAppliesAgainstRealAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("SKIPPED: KUBEBUILDER_ASSETS unset — the server-side-apply reproduction did not run")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	defer func() { _ = env.Stop() }()
	cl, err := client.New(cfg, client.Options{Scheme: testScheme(t)})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := context.Background()
	pr := basePR()
	if err := cl.Create(ctx, pr); err != nil {
		t.Fatalf("create: %v", err)
	}
	gvk := schema.GroupVersionKind{Group: pipelinev1alpha1.GroupVersion.Group, Version: pipelinev1alpha1.GroupVersion.Version, Kind: "PullRequest"}
	apply := func(status pipelinev1alpha1.PullRequestStatus) error {
		content, err := statusApplyContent(status)
		if err != nil {
			return err
		}
		patch := &unstructured.Unstructured{}
		patch.SetGroupVersionKind(gvk)
		patch.SetNamespace(pr.Namespace)
		patch.SetName(pr.Name)
		patch.UnstructuredContent()["status"] = content
		return cl.Status().Patch(ctx, patch, client.Apply, &client.PatchOptions{FieldManager: FIELD_MANAGER, Force: pointer.Bool(true)})
	}
	storedSourceBranches := func() string {
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvk)
		if err := cl.Get(ctx, client.ObjectKeyFromObject(pr), got); err != nil {
			t.Fatalf("get: %v", err)
		}
		status, _ := got.Object["status"].(map[string]interface{})
		b, _ := json.Marshal(status["sourceBranches"])
		return string(b)
	}

	one := pipelinev1alpha1.PullRequestStatus{ETag: "e1", SourceBranches: pipelinev1alpha1.Branches{
		Branches: []pipelinev1alpha1.Branch{{Name: "stale/branch", Commit: "sha-stale"}}}}
	if err := apply(one); err != nil {
		t.Fatalf("apply 1 branch: %v", err)
	}
	if got, want := storedSourceBranches(), `{"branches":[{"commit":"sha-stale","name":"stale/branch"}]}`; got != want {
		t.Fatalf("after 1-branch apply stored %s, want %s", got, want)
	}
	// Twice: the second empty apply is by a manager that now owns an empty list.
	for i := 0; i < 2; i++ {
		if err := apply(pipelinev1alpha1.PullRequestStatus{ETag: "e2"}); err != nil {
			t.Fatalf("empty-poll apply #%d rejected: %v", i+1, err)
		}
		if got, want := storedSourceBranches(), `{"branches":[]}`; got != want {
			t.Fatalf("after empty-poll apply #%d stored %s, want %s", i+1, got, want)
		}
	}
	// And back: a new PR after an empty period is recorded.
	if err := apply(one); err != nil {
		t.Fatalf("re-apply 1 branch: %v", err)
	}
	if got := storedSourceBranches(); got != `{"branches":[{"commit":"sha-stale","name":"stale/branch"}]}` {
		t.Fatalf("after re-apply stored %s", got)
	}
}
