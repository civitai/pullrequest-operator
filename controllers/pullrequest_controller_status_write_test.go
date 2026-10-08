package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	pipelinev1alpha1 "github.com/jquad-group/pullrequest-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// patchStubClient overrides only Status().Patch so the test controls the
// outcome of the status write deterministically.
type patchStubClient struct {
	client.Client
	patchErr error
	patches  *int
}

func (c patchStubClient) Status() client.StatusWriter {
	return patchStubStatusWriter{StatusWriter: c.Client.Status(), err: c.patchErr, patches: c.patches}
}

type patchStubStatusWriter struct {
	client.StatusWriter
	err     error
	patches *int
}

func (w patchStubStatusWriter) Patch(_ context.Context, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
	*w.patches++
	return w.err
}

// captureLogs returns a context carrying a logger that records every rendered
// log line (funcr renders Error() calls with an "error" key).
func captureLogs() (context.Context, func() []string) {
	var mu sync.Mutex
	var lines []string
	sink := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{})
	ctx := log.IntoContext(context.Background(), sink)
	return ctx, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

func errorLinesContaining(lines []string, needle string) int {
	n := 0
	for _, l := range lines {
		// funcr renders Error() calls with an "error" key; Info() never has one.
		if strings.Contains(l, `"error"=`) && strings.Contains(l, needle) {
			n++
		}
	}
	return n
}

func runReconcileWithPatch(t *testing.T, patchErr error) (time.Duration, error, []string, []string, int) {
	t.Helper()
	pr := basePR()
	poller := fakePoller{
		branches: pipelinev1alpha1.Branches{Branches: []pipelinev1alpha1.Branch{{Name: "feat/x", Commit: "sha-new"}}},
		etag:     "etag-new",
	}
	r, cl, rec := reconcilerFor(t, pr, poller)
	patches := 0
	r.Client = patchStubClient{Client: cl, patchErr: patchErr, patches: &patches}

	ctx, logs := captureLogs()
	res, err := r.Reconcile(ctx, reqFor(pr))

	var events []string
	for {
		select {
		case ev := <-rec.Events:
			events = append(events, ev)
			continue
		default:
		}
		break
	}
	return res.RequeueAfter, err, logs(), events, patches
}

// TestReconcileStatusWriteFailureIsLogged: when the status Patch is rejected
// (e.g. the object exceeds the API server / etcd size limit), Reconcile must log
// it at error level WITH the error text and emit a Warning event, while keeping
// the interval requeue so the next poll retries. Before this change the Patch
// error was discarded and nothing was logged.
func TestReconcileStatusWriteFailureIsLogged(t *testing.T) {
	const errText = "etcdserver: request is too large"
	requeueAfter, err, lines, events, patches := runReconcileWithPatch(t, errors.New(errText))

	if patches != 1 {
		t.Fatalf("expected exactly one status Patch attempt, got %d", patches)
	}
	if err != nil {
		t.Fatalf("Reconcile should keep returning a nil error (interval requeue), got %v", err)
	}
	if requeueAfter != 5*time.Minute {
		t.Fatalf("RequeueAfter = %v, want the spec interval 5m", requeueAfter)
	}
	if n := errorLinesContaining(lines, errText); n != 1 {
		t.Fatalf("want exactly 1 error-level log line carrying %q, got %d; logs=%q", errText, n, lines)
	}
	warned := false
	for _, ev := range events {
		if strings.HasPrefix(ev, "Warning StatusUpdateFailed") && strings.Contains(ev, errText) {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("want a Warning StatusUpdateFailed event carrying the error, got %q", events)
	}
}

// TestReconcileStatusWriteSuccessIsQuiet is the control arm: the same reconcile
// with a successful Patch logs no error and emits no StatusUpdateFailed event.
func TestReconcileStatusWriteSuccessIsQuiet(t *testing.T) {
	requeueAfter, err, lines, events, patches := runReconcileWithPatch(t, nil)
	if patches != 1 {
		t.Fatalf("expected exactly one status Patch attempt, got %d", patches)
	}
	if err != nil || requeueAfter != 5*time.Minute {
		t.Fatalf("got (%v, %v), want (5m, nil)", requeueAfter, err)
	}
	for _, l := range lines {
		if strings.Contains(l, `"error"=`) {
			t.Fatalf("no error log expected on a successful write, got %q", l)
		}
	}
	for _, ev := range events {
		if strings.Contains(ev, "StatusUpdateFailed") {
			t.Fatalf("no StatusUpdateFailed event expected, got %q", ev)
		}
	}
}

// TestWarnIfStatusLarge: the size guard logs an error above 1 MiB and stays
// silent below it.
func TestWarnIfStatusLarge(t *testing.T) {
	mk := func(detailsBytes int) *pipelinev1alpha1.PullRequest {
		pr := basePR()
		pr.Status.SourceBranches.Branches = []pipelinev1alpha1.Branch{{
			Name: "b", Commit: "c", Details: strings.Repeat("x", detailsBytes),
		}}
		return pr
	}

	ctx, logs := captureLogs()
	if size := warnIfStatusLarge(ctx, mk(1000)); size <= 1000 || size > statusSizeWarnBytes {
		t.Fatalf("small status measured %d bytes", size)
	}
	if n := errorLinesContaining(logs(), "approaching the etcd object size limit"); n != 0 {
		t.Fatalf("small status should not warn, got %d error lines", n)
	}

	ctx, logs = captureLogs()
	size := warnIfStatusLarge(ctx, mk(statusSizeWarnBytes+1))
	if size <= statusSizeWarnBytes {
		t.Fatalf("large status measured only %d bytes", size)
	}
	if n := errorLinesContaining(logs(), fmt.Sprintf("status is %d bytes", size)); n != 1 {
		t.Fatalf("large status should log exactly 1 error line with its size, got %d; logs=%q", n, logs())
	}
}
