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
	current = store3

	// Reconcile 4: an idempotent poll — the SAME open-PR set comes back. Store
	// must remain the full set and NO PR must be reported as new (no spurious
	// events on a steady-state poll).
	polled4 := branchesOf("a", "b")
	store4, added4 := nextSourceBranches(current, polled4)
	if got, want := names(store4.Branches), []string{"a", "b"}; !equalStringSets(got, want) {
		t.Fatalf("no-change: stored=%v, want FULL list %v", got, want)
	}
	if len(added4) != 0 {
		t.Fatalf("no-change: newlyAdded=%v, want none on a steady-state poll", names(added4))
	}
}

// branchWithCommit builds a single-branch Branches where Name and Commit are set
// independently — needed to model a label change that alters ONLY the commit
// discriminator on an otherwise-unchanged branch.
func branchWithCommit(name, commit string) pipelinev1alpha1.Branches {
	return pipelinev1alpha1.Branches{Branches: []pipelinev1alpha1.Branch{{Name: name, Commit: commit}}}
}

// TestNextSourceBranchesLabelChangeSameBranch models the label-retrigger core at
// the store-decision layer: a PR on branch "feat/x" gets a label, so its Commit
// discriminator changes from "<sha>" to "<sha>-<labelhash>" while the branch Name
// stays the same. The store must become the FULL new set (the new discriminator),
// and the branch must be reported as newly-added so the downstream consumer
// refires the build for the new label state — while a NON-change poll must report
// nothing (no spurious retrigger).
func TestNextSourceBranchesLabelChangeSameBranch(t *testing.T) {
	current := branchWithCommit("feat/x", "sha")

	// Label added -> commit discriminator changes -> appears newly-added.
	polled := branchWithCommit("feat/x", "sha-labelhash")
	store, added := nextSourceBranches(current, polled)
	if len(store.Branches) != 1 || store.Branches[0].Commit != "sha-labelhash" {
		t.Fatalf("label change: store=%v, want the new discriminator sha-labelhash", store.Branches)
	}
	if len(added) != 1 || added[0].Name != "feat/x" || added[0].Commit != "sha-labelhash" {
		t.Fatalf("label change: newlyAdded=%v, want feat/x@sha-labelhash (retrigger)", added)
	}
	current = store

	// Poll again with the SAME discriminator -> no retrigger.
	polledSame := branchWithCommit("feat/x", "sha-labelhash")
	storeSame, addedSame := nextSourceBranches(current, polledSame)
	if len(storeSame.Branches) != 1 || storeSame.Branches[0].Commit != "sha-labelhash" {
		t.Fatalf("stable: store=%v, want unchanged discriminator", storeSame.Branches)
	}
	if len(addedSame) != 0 {
		t.Fatalf("stable: newlyAdded=%v, want none (no spurious retrigger)", names(addedSame))
	}
}
