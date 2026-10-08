/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"
	pipelinev1alpha1 "github.com/jquad-group/pullrequest-operator/api/v1alpha1"
	gitApi "github.com/jquad-group/pullrequest-operator/pkg/git"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	FIELD_MANAGER = "pullrequest-controller"

	BITBUCKET_PROVIDER_NAME = "Bitbucket"
	GITHUB_PROVIDER_NAME    = "Github"

	// Status
	ReconcileUnknown       = "Unknown"
	ReconcileError         = "Error"
	ReconcileErrorReason   = "Failed"
	ReconcileSuccess       = "Success"
	ReconcileSuccessReason = "Succeded"

	// Bitbucket and Github Secret Key
	SECRET_ACCESSTOKEN_KEY = "accessToken"
)

// PullRequestReconciler reconciles a PullRequest object
type PullRequestReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	recorder record.EventRecorder
	// newPoller builds the git poller for a PullRequest. It defaults to
	// createGitPoller (a live GitHub/Bitbucket poller) and exists only as a test
	// seam so Reconcile can be exercised with a fake poller without a network
	// endpoint. It is never set in production.
	newPoller func(*pipelinev1alpha1.PullRequest, string) gitApi.PullrequestPoller
}

//+kubebuilder:rbac:groups=pipeline.jquad.rocks,resources=pullrequests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=pipeline.jquad.rocks,resources=pullrequests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=pipeline.jquad.rocks,resources=pullrequests/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch;update;get;list;watch

func (r *PullRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	//log := log.FromContext(ctx)

	var pullrequest pipelinev1alpha1.PullRequest
	if err := r.Get(ctx, req.NamespacedName, &pullrequest); err != nil {
		// return and dont requeue
		return ctrl.Result{}, nil
	}

	patch := &unstructured.Unstructured{}
	patch.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   pipelinev1alpha1.GroupVersion.Group,
		Version: pipelinev1alpha1.GroupVersion.Version,
		Kind:    "PullRequest",
	})
	patch.SetNamespace(pullrequest.GetNamespace())
	patch.SetName(pullrequest.GetName())
	patchOptions := &client.PatchOptions{
		FieldManager: FIELD_MANAGER,
		Force:        pointer.Bool(true),
	}

	// newPoller defaults to the real createGitPoller; a test may inject a fake.
	newPoller := r.newPoller
	if newPoller == nil {
		newPoller = createGitPoller
	}

	var prPoller gitApi.PullrequestPoller
	// Credentials for Github/Bitbucket are provided
	if len(pullrequest.Spec.GitProvider.SecretRef) > 0 {
		// try to find the provided secret on the cluster
		foundSecret := &v1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: pullrequest.Spec.GitProvider.SecretRef, Namespace: pullrequest.Namespace}, foundSecret); err != nil {
			return r.ManageError(ctx, &pullrequest, req, err)
		}
		// validate the secret's format
		if err := Validate(&pullrequest, *foundSecret); err != nil {
			r.recorder.Event(&pullrequest, v1.EventTypeWarning, "Error", err.Error())
			return r.ManageError(ctx, &pullrequest, req, err)
		}
		prPoller = newPoller(&pullrequest, string(foundSecret.Data[SECRET_ACCESSTOKEN_KEY]))
	} else {
		prPoller = newPoller(&pullrequest, "")
	}

	newBranches, eTag, err := prPoller.Poll(pullrequest.Spec.TargetBranch.Name, pullrequest.Status.ETag)
	if (eTag == pullrequest.Status.ETag) && (eTag != "") {
		// Request returned 304 Not Modified, return and requeue at the specified interval
		return ctrl.Result{RequeueAfter: pullrequest.Spec.Interval.Duration}, nil
	}
	if err != nil {
		r.recorder.Event(&pullrequest, v1.EventTypeWarning, "Error", err.Error())
		return r.ManageError(ctx, &pullrequest, req, err)
	}

	if !pullrequest.Status.SourceBranches.Equals(newBranches) {
		store, newlyAdded := nextSourceBranches(pullrequest.Status.SourceBranches, newBranches)
		for i := 0; i < len(newlyAdded); i++ {
			r.recorder.Event(&pullrequest, v1.EventTypeNormal, "Info", "New PR "+newlyAdded[i].Name+"/"+newlyAdded[i].Commit+" received.")
		}
		condition := metav1.Condition{
			Type:               ReconcileSuccess,
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: pullrequest.GetGeneration(),
			Reason:             ReconcileSuccessReason,
			Status:             metav1.ConditionTrue,
			Message:            "Success",
		}
		pullrequest.AddOrReplaceCondition(condition)
		pullrequest.Status.ETag = eTag
		pullrequest.Status.SourceBranches.Branches = store.Branches
		warnIfStatusLarge(ctx, &pullrequest)
		statusContent, err := statusApplyContent(pullrequest.Status)
		if err == nil {
			patch.UnstructuredContent()["status"] = statusContent
			err = r.Status().Patch(ctx, patch, client.Apply, patchOptions)
		}
		if err != nil {
			// Previously this error was discarded, so a status write rejected by the
			// API server (e.g. the object exceeding etcd's size limit) left the CR
			// silently stale while new PRs/commits were still logged as received.
			// Keep the interval requeue (the next poll re-diffs and retries the
			// write), but make the failure visible.
			log.FromContext(ctx).Error(err, "unable to update PullRequest status; new PRs/commits are NOT recorded",
				"pullrequest", req.NamespacedName.String(), "branches", len(store.Branches))
			r.recorder.Event(&pullrequest, v1.EventTypeWarning, "StatusUpdateFailed", err.Error())
		}
	}

	return ctrl.Result{RequeueAfter: pullrequest.Spec.Interval.Duration}, nil
}

// nextSourceBranches computes the next authoritative Status.SourceBranches state
// and the set of newly-added branches, given the currently-stored branches
// (current) and the freshly-polled FULL open-PR set (polled).
//
// The stored state is ALWAYS the full polled list: Status.SourceBranches is the
// authoritative complete open-PR set that the downstream consumer
// (pipeline-trigger-operator) reads. newlyAdded contains only the branches present
// in polled but NOT in current, so events fire only for genuinely-new PRs.
//
// Storing only the delta (the historical bug) truncated SourceBranches to the
// newly-added branches — empty when a PR closed — causing the downstream consumer
// to rebuild every open PR and the next poll to re-diff against the truncated
// state, churning endlessly. See production incident 2026-07-06.
func nextSourceBranches(current, polled pipelinev1alpha1.Branches) (store pipelinev1alpha1.Branches, newlyAdded []pipelinev1alpha1.Branch) {
	newlyAdded = current.BranchSetDifference(polled)
	store = pipelinev1alpha1.Branches{Branches: polled.Branches}
	return store, newlyAdded
}

// statusApplyContent renders status as the server-side-apply body for the status
// subresource, always carrying sourceBranches.branches as an explicit array —
// empty ([]) when the poll found no open PRs.
//
// Branches.Branches is `omitempty`, so an empty poll serialises as
// `"sourceBranches":{}`. When this field manager already owns
// sourceBranches.branches (any earlier successful write), the API server's
// server-side apply of that empty object yields sourceBranches = null, which
// the CRD schema (type: object) rejects: `status.sourceBranches: Invalid value:
// "null"`. The whole status write then fails and the CR keeps its last
// non-empty branch list forever. An explicit `"branches": []` is accepted and
// stored as {"branches": []}. (Reproduced against kube-apiserver 1.33 via
// envtest — see TestEmptyStatusAppliesAgainstRealAPIServer.)
func statusApplyContent(status pipelinev1alpha1.PullRequestStatus) (map[string]interface{}, error) {
	raw, err := json.Marshal(status)
	if err != nil {
		return nil, err
	}
	content := map[string]interface{}{}
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, err
	}
	sb, _ := content["sourceBranches"].(map[string]interface{})
	if sb == nil {
		sb = map[string]interface{}{}
		content["sourceBranches"] = sb
	}
	if _, ok := sb["branches"]; !ok {
		sb["branches"] = []interface{}{}
	}
	return content, nil
}

// statusSizeWarnBytes is the serialized-status size above which Reconcile logs
// an error before writing. etcd rejects objects over ~1.5 MiB by default, so
// 1 MiB leaves headroom to notice growth before writes start failing.
const statusSizeWarnBytes = 1 << 20

// warnIfStatusLarge logs (does not block) when the serialized status exceeds
// statusSizeWarnBytes. It returns the measured size for tests.
func warnIfStatusLarge(ctx context.Context, pr *pipelinev1alpha1.PullRequest) int {
	raw, err := json.Marshal(pr.Status)
	if err != nil {
		return 0
	}
	if len(raw) > statusSizeWarnBytes {
		log.FromContext(ctx).Error(fmt.Errorf("status is %d bytes (warn threshold %d)", len(raw), statusSizeWarnBytes),
			"PullRequest status is approaching the etcd object size limit",
			"pullrequest", pr.Namespace+"/"+pr.Name, "branches", len(pr.Status.SourceBranches.Branches))
	}
	return len(raw)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PullRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.recorder = mgr.GetEventRecorderFor("PullRequest")

	return ctrl.NewControllerManagedBy(mgr).
		For(&pipelinev1alpha1.PullRequest{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

func (r *PullRequestReconciler) ManageError(context context.Context, obj *pipelinev1alpha1.PullRequest, req ctrl.Request, message error) (reconcile.Result, error) {
	log := log.FromContext(context)
	if err := r.Get(context, types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}, obj); err != nil {
		log.Error(err, "unable to get obj")
		return reconcile.Result{}, err
	}

	condition := metav1.Condition{
		Type:               ReconcileError,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: obj.GetGeneration(),
		Reason:             ReconcileErrorReason,
		Status:             metav1.ConditionFalse,
		Message:            message.Error(),
	}
	obj.AddOrReplaceCondition(condition)
	err := r.Status().Update(context, obj)
	if err != nil {
		log.Error(err, "unable to update status")
		return reconcile.Result{}, err
	}
	return reconcile.Result{Requeue: true}, nil
}

func Validate(pullrequest *pipelinev1alpha1.PullRequest, secret v1.Secret) error {
	if len(secret.Data[SECRET_ACCESSTOKEN_KEY]) <= 0 {
		return fmt.Errorf("invalid HTTP auth option: 'accessToken' must be set")
	}
	return nil
}

func createGitPoller(repo *pipelinev1alpha1.PullRequest, accessToken string) gitApi.PullrequestPoller {
	switch repo.Spec.GitProvider.Provider {
	case GITHUB_PROVIDER_NAME:
		return gitApi.NewGithubPoller(repo.Spec.GitProvider.Github.Url, accessToken, repo.Spec.GitProvider.InsecureSkipVerify, repo.Spec.GitProvider.Github.Owner, repo.Spec.GitProvider.Github.Repository)
	case BITBUCKET_PROVIDER_NAME:
		return gitApi.NewBitbucketPoller(repo.Spec.GitProvider.Bitbucket.RestEndpoint, accessToken, repo.Spec.GitProvider.InsecureSkipVerify, repo.Spec.GitProvider.Bitbucket.Project, repo.Spec.GitProvider.Bitbucket.Repository)
	}
	return nil
}
