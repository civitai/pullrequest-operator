package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	pipelinev1alpha1 "github.com/jquad-group/pullrequest-operator/api/v1alpha1"
	gitApi "github.com/jquad-group/pullrequest-operator/pkg/git"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestValidate covers the secret-format check on the credentialed poll path: a
// missing/empty accessToken is rejected, a present one passes.
func TestValidate(t *testing.T) {
	pr := basePR()
	if err := Validate(pr, corev1.Secret{}); err == nil {
		t.Fatal("empty secret should fail validation")
	}
	if err := Validate(pr, corev1.Secret{Data: map[string][]byte{SECRET_ACCESSTOKEN_KEY: {}}}); err == nil {
		t.Fatal("empty accessToken should fail validation")
	}
	if err := Validate(pr, corev1.Secret{Data: map[string][]byte{SECRET_ACCESSTOKEN_KEY: []byte("tok")}}); err != nil {
		t.Fatalf("valid accessToken should pass, got %v", err)
	}
}

// fakePoller is a stub PullrequestPoller returning canned results.
type fakePoller struct {
	branches pipelinev1alpha1.Branches
	etag     string
	err      error
}

func (f fakePoller) Poll(branch, etag string) (pipelinev1alpha1.Branches, string, error) {
	return f.branches, f.etag, f.err
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := pipelinev1alpha1.AddToScheme(sch); err != nil {
		t.Fatalf("add pipeline scheme: %v", err)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return sch
}

func basePR() *pipelinev1alpha1.PullRequest {
	return &pipelinev1alpha1.PullRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pr", Namespace: "default"},
		Spec: pipelinev1alpha1.PullRequestSpec{
			GitProvider:  pipelinev1alpha1.GitProvider{Provider: GITHUB_PROVIDER_NAME},
			TargetBranch: pipelinev1alpha1.Branch{Name: "main"},
			Interval:     metav1.Duration{Duration: 5 * time.Minute},
		},
	}
}

func reconcilerFor(t *testing.T, pr *pipelinev1alpha1.PullRequest, poller gitApi.PullrequestPoller) (*PullRequestReconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	sch := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(pr).Build()
	rec := record.NewFakeRecorder(16)
	r := &PullRequestReconciler{
		Client:   cl,
		Scheme:   sch,
		recorder: rec,
		newPoller: func(_ *pipelinev1alpha1.PullRequest, _ string) gitApi.PullrequestPoller {
			return poller
		},
	}
	return r, cl, rec
}

func reqFor(pr *pipelinev1alpha1.PullRequest) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: pr.Name, Namespace: pr.Namespace}}
}

// TestReconcileObjectNotFound: a Get miss returns cleanly without requeue and
// without touching the poller (deleted CR must not error/loop).
func TestReconcileObjectNotFound(t *testing.T) {
	sch := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &PullRequestReconciler{Client: cl, Scheme: sch, recorder: record.NewFakeRecorder(4)}

	res, err := r.Reconcile(context.Background(), reqFor(basePR()))
	if err != nil {
		t.Fatalf("not-found should not error, got %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Fatalf("not-found should not requeue, got %+v", res)
	}
}

// TestReconcileETagNotModified: when the poller returns the SAME ETag already in
// status, Reconcile early-returns a requeue and performs NO status write and NO
// event — the churn guard. Verifies the stored SourceBranches are untouched.
func TestReconcileETagNotModified(t *testing.T) {
	pr := basePR()
	pr.Status.ETag = "same-etag"
	pr.Status.SourceBranches = pipelinev1alpha1.Branches{Branches: []pipelinev1alpha1.Branch{{Name: "keep", Commit: "keep"}}}

	// Poller returns a DIFFERENT branch set but the SAME etag: the etag guard must
	// win and prevent any write of the new set.
	poller := fakePoller{
		branches: pipelinev1alpha1.Branches{Branches: []pipelinev1alpha1.Branch{{Name: "new", Commit: "new"}}},
		etag:     "same-etag",
	}
	r, cl, rec := reconcilerFor(t, pr, poller)

	res, err := r.Reconcile(context.Background(), reqFor(pr))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 5*time.Minute {
		t.Fatalf("RequeueAfter = %v, want the spec interval 5m", res.RequeueAfter)
	}
	select {
	case ev := <-rec.Events:
		t.Fatalf("no event expected on a 304, got %q", ev)
	default:
	}
	var got pipelinev1alpha1.PullRequest
	if err := cl.Get(context.Background(), reqFor(pr).NamespacedName, &got); err != nil {
		t.Fatalf("get after reconcile: %v", err)
	}
	if len(got.Status.SourceBranches.Branches) != 1 || got.Status.SourceBranches.Branches[0].Name != "keep" {
		t.Fatalf("304 must not overwrite SourceBranches, got %v", got.Status.SourceBranches.Branches)
	}
}

// TestReconcilePollErrorPreservesBranches: a failed poll must set an Error
// condition and requeue, and must NOT wipe the previously-stored SourceBranches
// (the downstream consumer keeps building the last-known-good open-PR set).
func TestReconcilePollErrorPreservesBranches(t *testing.T) {
	pr := basePR()
	pr.Status.SourceBranches = pipelinev1alpha1.Branches{Branches: []pipelinev1alpha1.Branch{
		{Name: "a", Commit: "a"}, {Name: "b", Commit: "b"},
	}}
	poller := fakePoller{err: errors.New("boom")}
	r, cl, rec := reconcilerFor(t, pr, poller)

	res, err := r.Reconcile(context.Background(), reqFor(pr))
	if err != nil {
		t.Fatalf("ManageError should return nil error (requeue), got %v", err)
	}
	if !res.Requeue {
		t.Fatalf("poll error should requeue, got %+v", res)
	}
	// A Warning event is emitted for the error.
	select {
	case <-rec.Events:
	default:
		t.Fatal("expected a Warning event on poll error")
	}

	var got pipelinev1alpha1.PullRequest
	if err := cl.Get(context.Background(), reqFor(pr).NamespacedName, &got); err != nil {
		t.Fatalf("get after reconcile: %v", err)
	}
	if len(got.Status.SourceBranches.Branches) != 2 {
		t.Fatalf("poll error must NOT wipe SourceBranches, got %v", got.Status.SourceBranches.Branches)
	}
	cond, ok := got.GetCondition(ReconcileError)
	if !ok {
		t.Fatalf("expected an %q condition after poll error, conditions=%v", ReconcileError, got.Status.Conditions)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("error condition status = %v, want False", cond.Status)
	}
}
