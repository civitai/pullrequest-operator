package controllers

import (
	"testing"

	pipelinev1alpha1 "github.com/jquad-group/pullrequest-operator/api/v1alpha1"
)

// branchesOf builds a Branches from bare PR names (Commit == name for simplicity).
func branchesOf(names ...string) pipelinev1alpha1.Branches {
	b := make([]pipelinev1alpha1.Branch, 0, len(names))
	for _, n := range names {
		b = append(b, pipelinev1alpha1.Branch{Name: n, Commit: n})
	}
	return pipelinev1alpha1.Branches{Branches: b}
}

func names(bs []pipelinev1alpha1.Branch) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return out
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, x := range a {
		m[x]++
	}
	for _, y := range b {
		m[y]--
		if m[y] < 0 {
			return false
		}
	}
	return true
}

// TestNextSourceBranches drives the store-decision the way Reconcile does across
// successive polls: initial population, an ADD, and a REMOVE. It asserts that
// Status.SourceBranches always ends up equal to the FULL polled open-PR set (not
// the delta), while events fire only for genuinely-new PRs.
//
// This test FAILS against the original `= setDifferences` (delta-only) code: the
// add case would store only {c} and the remove case would store {} (empty),
// neither of which equals the full polled list.
func TestNextSourceBranches(t *testing.T) {
	// Simulate the reconciler threading the previously-stored state into the
	// next poll, as pullrequest.Status.SourceBranches does across reconciles.
	current := pipelinev1alpha1.Branches{} // fresh object: no branches stored yet

	// Reconcile 1: initial population — poller returns {a, b}.
	polled1 := branchesOf("a", "b")
	store1, added1 := nextSourceBranches(current, polled1)
	if got, want := names(store1.Branches), []string{"a", "b"}; !equalStringSets(got, want) {
		t.Fatalf("initial: stored=%v, want full polled list %v", got, want)
	}
	if got, want := names(added1), []string{"a", "b"}; !equalStringSets(got, want) {
		t.Fatalf("initial: newlyAdded=%v, want %v (all are new)", got, want)
	}
	current = store1

	// Reconcile 2: a PR is opened — poller returns {a, b, c}.
	polled2 := branchesOf("a", "b", "c")
	store2, added2 := nextSourceBranches(current, polled2)
	if got, want := names(store2.Branches), []string{"a", "b", "c"}; !equalStringSets(got, want) {
		t.Fatalf("add: stored=%v, want FULL list %v (not just the delta)", got, want)
	}
	if got, want := names(added2), []string{"c"}; !equalStringSets(got, want) {
		t.Fatalf("add: newlyAdded=%v, want only the new PR %v", got, want)
	}
	current = store2

	// Reconcile 3: a PR is closed — poller returns {a, b}. This is the bug's
	// worst case: BranchSetDifference is empty, so the delta-only code would
	// store {} and blank out the authoritative set.
	polled3 := branchesOf("a", "b")
	store3, added3 := nextSourceBranches(current, polled3)
	if got, want := names(store3.Branches), []string{"a", "b"}; !equalStringSets(got, want) {
		t.Fatalf("remove: stored=%v, want FULL remaining list %v (delta would blank it)", got, want)
	}
	if len(added3) != 0 {
		t.Fatalf("remove: newlyAdded=%v, want none (no genuinely-new PR)", names(added3))
	}
}
