package v1alpha1

import "testing"

// br is a terse Branch constructor for the tables below.
func br(name, sha, commit string) Branch {
	return Branch{Name: name, SHA: sha, Commit: commit}
}

// TestBranchEquals pins the contract of Branch.Equals: it compares Name, SHA and
// Commit and DELIBERATELY IGNORES Details. This is load-bearing for the
// label-retrigger fork — the label hash is folded into Commit precisely because
// Details (the marshaled PR JSON, which changes on any PR edit) is not compared,
// so only a real commit/label change is seen as a difference.
func TestBranchEquals(t *testing.T) {
	base := br("feat/x", "sha1", "sha1")

	cases := []struct {
		name string
		a, b Branch
		want bool
	}{
		{"identical", base, br("feat/x", "sha1", "sha1"), true},
		{"different name", base, br("feat/y", "sha1", "sha1"), false},
		{"different sha", base, br("feat/x", "sha2", "sha1"), false},
		{"different commit (label folded in)", base, br("feat/x", "sha1", "sha1-deadbeef"), false},
		{
			// Details differs but Name/SHA/Commit match -> Equals TRUE.
			// This is the guard that stops a PR-body/label metadata edit from
			// churning the status when the commit discriminator is unchanged.
			name: "details differ only",
			a:    Branch{Name: "feat/x", SHA: "sha1", Commit: "sha1", Details: `{"body":"old"}`},
			b:    Branch{Name: "feat/x", SHA: "sha1", Commit: "sha1", Details: `{"body":"new"}`},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.a
			if got := a.Equals(tc.b); got != tc.want {
				t.Fatalf("Branch.Equals(%+v, %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestBranchesEquals pins the (quirky) contract of Branches.Equals as the code
// actually behaves — Reconcile's churn guard depends on it:
//   - an EMPTY receiver always returns false (even vs another empty set),
//   - it is ORDER-SENSITIVE (element-wise by index), and
//   - it compares element-wise via Branch.Equals (so Details is ignored).
func TestBranchesEquals(t *testing.T) {
	mk := func(bs ...Branch) Branches { return Branches{Branches: bs} }
	a := br("a", "sa", "ca")
	b := br("b", "sb", "cb")

	cases := []struct {
		name        string
		recv, other Branches
		want        bool
	}{
		{"empty receiver vs empty -> false (quirk)", mk(), mk(), false},
		{"empty receiver vs non-empty -> false", mk(), mk(a), false},
		{"identical same order -> true", mk(a, b), mk(a, b), true},
		{"reordered -> false (order-sensitive)", mk(a, b), mk(b, a), false},
		{"different size -> false", mk(a, b), mk(a), false},
		{"one element differs -> false", mk(a, b), mk(a, br("b", "sb", "cb2")), false},
		{
			// same Name/SHA/Commit, different Details -> still equal (Details ignored).
			name:  "details differ only -> true",
			recv:  mk(Branch{Name: "a", SHA: "sa", Commit: "ca", Details: "x"}),
			other: mk(Branch{Name: "a", SHA: "sa", Commit: "ca", Details: "y"}),
			want:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recv := tc.recv
			if got := recv.Equals(tc.other); got != tc.want {
				t.Fatalf("Branches.Equals: got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBranchSetDifference pins the directional set-difference used to decide
// which PRs are "newly added" (fire an event). Contract:
// receiver.BranchSetDifference(polled) == { items in polled NOT in receiver },
// keyed by the FULL Branch struct (so a changed Commit or Details makes an entry
// "new"). Removed items are NOT reported (it is one-directional).
func TestBranchSetDifference(t *testing.T) {
	mk := func(bs ...Branch) Branches { return Branches{Branches: bs} }
	a := br("a", "sa", "ca")
	b := br("b", "sb", "cb")
	c := br("c", "sc", "cc")

	// names extracts the Name of each diff element for order-insensitive compare.
	nameSet := func(bs []Branch) map[string]int {
		m := map[string]int{}
		for _, x := range bs {
			m[x.Name]++
		}
		return m
	}
	eq := func(got []Branch, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		g := nameSet(got)
		for _, w := range want {
			g[w]--
			if g[w] < 0 {
				return false
			}
		}
		return true
	}

	t.Run("disjoint", func(t *testing.T) {
		recv := mk(a)
		if d := recv.BranchSetDifference(mk(b)); !eq(d, "b") {
			t.Fatalf("disjoint: got %v, want [b]", names2(d))
		}
	})
	t.Run("subset polled is superset -> only the added one", func(t *testing.T) {
		recv := mk(a, b)
		if d := recv.BranchSetDifference(mk(a, b, c)); !eq(d, "c") {
			t.Fatalf("add: got %v, want [c]", names2(d))
		}
	})
	t.Run("shrink -> empty (removals are not reported)", func(t *testing.T) {
		recv := mk(a, b, c)
		if d := recv.BranchSetDifference(mk(a, b)); len(d) != 0 {
			t.Fatalf("shrink: got %v, want []", names2(d))
		}
	})
	t.Run("identical -> empty", func(t *testing.T) {
		recv := mk(a, b)
		if d := recv.BranchSetDifference(mk(a, b)); len(d) != 0 {
			t.Fatalf("identical: got %v, want []", names2(d))
		}
	})
	t.Run("reordered -> empty (map-keyed, order-insensitive)", func(t *testing.T) {
		recv := mk(a, b)
		if d := recv.BranchSetDifference(mk(b, a)); len(d) != 0 {
			t.Fatalf("reordered: got %v, want [] (unlike Equals)", names2(d))
		}
	})
	t.Run("same commit different branch name -> new", func(t *testing.T) {
		// Two PRs sharing a head SHA/commit but on different branches are
		// distinct entries (different Name -> different struct key).
		recv := mk(br("x", "s", "shared"))
		d := recv.BranchSetDifference(mk(br("y", "s", "shared")))
		if !eq(d, "y") {
			t.Fatalf("dup-commit-diff-branch: got %v, want [y]", names2(d))
		}
	})
	t.Run("label-folded commit change -> same branch appears new", func(t *testing.T) {
		// The label-retrigger core, at the set level: same branch, SHA folded
		// with a label hash into Commit -> a different struct key -> reported as
		// newly-added so downstream refires.
		recv := mk(br("feat/x", "sha", "sha"))
		d := recv.BranchSetDifference(mk(br("feat/x", "sha", "sha-labelhash")))
		if !eq(d, "feat/x") {
			t.Fatalf("label change: got %v, want [feat/x]", names2(d))
		}
	})
}

func names2(bs []Branch) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return out
}

// TestSetGetBranchesAndSize covers the trivial accessors used by the reconciler.
func TestSetGetBranchesAndSize(t *testing.T) {
	var bs Branches
	if bs.GetSize() != 0 {
		t.Fatalf("empty GetSize = %d, want 0", bs.GetSize())
	}
	in := []Branch{br("a", "s", "c"), br("b", "s", "c")}
	bs.SetBranches(in)
	if bs.GetSize() != 2 {
		t.Fatalf("GetSize after SetBranches = %d, want 2", bs.GetSize())
	}
	got := bs.GetBranches()
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("GetBranches = %v, want [a b]", names2(got))
	}
}
