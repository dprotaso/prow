/*
Copyright 2026 The Kubernetes Authors.

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

package repoowners

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/prow/pkg/git/localgit"
	"sigs.k8s.io/prow/pkg/github"
)

func TestTeamResolver(t *testing.T) {
	ghc := &fakeGitHubClient{teams: map[string][]github.TeamMember{
		"org/maintainers": {{Login: "Alice"}, {Login: "bob"}},
		"org/reviewers":   {{Login: "bob"}, {Login: "carol"}},
	}}
	aliases, err := ParseAliasesConfig([]byte(`aliases:
  reviewers:
  - "@ORG/maintainers"
  - "@org/reviewers"
  - alice
`))
	if err != nil {
		t.Fatal(err)
	}
	input := aliases.ExpandAliases(NormLogins([]string{"reviewers", "@org/maintainers", "@dave"}))
	original := input.Clone()
	resolver := NewTeamResolver("ORG", ghc)
	for i := 0; i < 2; i++ {
		got, err := resolver.Resolve(input)
		if err != nil {
			t.Fatal(err)
		}
		if want := sets.New("alice", "bob", "carol", "dave"); !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
		// Mutating a result must not corrupt the resolver's membership snapshot.
		got.Delete("bob")
	}
	if !input.Equal(original) {
		t.Fatal("resolver mutated its input")
	}
	if ghc.teamCalls != 2 {
		t.Errorf("got %d API calls, want one per distinct team", ghc.teamCalls)
	}
}

func TestTeamResolverErrors(t *testing.T) {
	lookupErr := errors.New("membership unavailable")
	for _, tc := range []struct {
		name, owner, want string
		apiErr            error
		calls             int
	}{
		{name: "missing slug", owner: "@org/", want: "expected @org/team-slug"},
		{name: "extra slash", owner: "@org/team/extra", want: "expected @org/team-slug"},
		{name: "different organization", owner: "@other/team", want: "repository organization"},
		{name: "lookup error", owner: "@org/team", want: lookupErr.Error(), apiErr: lookupErr, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ghc := &fakeGitHubClient{teamErr: tc.apiErr}
			got, err := NewTeamResolver("org", ghc).Resolve(sets.New(tc.owner))
			if err == nil || !strings.Contains(err.Error(), tc.want) || got != nil {
				t.Fatalf("got %v, %v; want error containing %q", got, err, tc.want)
			}
			if tc.apiErr != nil && !errors.Is(err, tc.apiErr) {
				t.Errorf("lookup error was not preserved: %v", err)
			}
			if ghc.teamCalls != tc.calls {
				t.Errorf("got %d calls, want %d", ghc.teamCalls, tc.calls)
			}
		})
	}
}

func TestTeamReferenceCannotBeAliasName(t *testing.T) {
	if _, err := ParseAliasesConfig([]byte("aliases:\n  '@org/team': [alice]\n")); err == nil {
		t.Fatal("expected reserved alias name to be rejected")
	}
}

func TestLoadRepoOwnersRefreshesTeams(t *testing.T) {
	for _, skipCollaborators := range []bool{false, true} {
		t.Run(map[bool]string{false: "filter collaborators", true: "skip collaborators"}[skipCollaborators], func(t *testing.T) {
			files := map[string][]byte{
				"OWNERS": []byte(`approvers:
- maintainers
reviewers:
- "@org/team"
labels:
- area/team
`),
				"OWNERS_ALIASES": []byte("aliases:\n  maintainers:\n  - '@org/team'\n"),
				"sub/OWNERS": []byte(`options:
  no_parent_owners: true
filters:
  '\.go$':
    approvers:
    - "@org/team"
`),
			}
			client, cleanup, err := getTestClient(files, false, skipCollaborators, false, false, nil, nil, nil, nil, localgit.NewV2)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup)
			ghc := client.ghc.(*fakeGitHubClient)
			ghc.teams = map[string][]github.TeamMember{"org/team": {{Login: "alice"}, {Login: "outsider"}}}
			first, err := client.LoadRepoOwners("org", "repo", defaultBranch)
			if err != nil {
				t.Fatal(err)
			}
			want := sets.New("alice")
			if skipCollaborators {
				want.Insert("outsider")
			}
			if !first.LeafApprovers("file.go").Equal(want) || !first.LeafReviewers("file.go").Equal(want) {
				t.Fatalf("unexpected owners: %v, %v", first.LeafApprovers("file.go"), first.LeafReviewers("file.go"))
			}
			if !first.LeafApprovers("sub/file.go").Equal(want) {
				t.Fatal("team resolution did not preserve filtered ownership")
			}
			for re, owners := range first.(*RepoOwners).approvers["sub"] {
				if re == nil || !re.MatchString("file.go") || re.MatchString("file.txt") || !owners.Equal(want) {
					t.Fatal("team resolution changed the ownership filter")
				}
			}
			if !first.IsNoParentOwners("sub") || !first.FindLabelsForFile("file.go").Has("area/team") {
				t.Fatal("team resolution changed options or labels")
			}

			// No commit changes: the next load must still observe changed membership.
			ghc.teams["org/team"] = []github.TeamMember{{Login: "bob"}}
			second, err := client.LoadRepoOwners("org", "repo", defaultBranch)
			if err != nil {
				t.Fatal(err)
			}
			if !second.LeafApprovers("file.go").Equal(sets.New("bob")) || !second.AllReviewers().Equal(sets.New("bob")) {
				t.Fatalf("membership was cached by SHA: %v", second.AllOwners())
			}
			if !first.LeafApprovers("file.go").Equal(want) {
				t.Fatal("refresh mutated previously returned ownership snapshot")
			}
			if ghc.teamCalls != 2 {
				t.Errorf("got %d calls, want one per load", ghc.teamCalls)
			}
			ghc.teamErr = errors.New("GitHub unavailable")
			if owners, err := client.LoadRepoOwners("org", "repo", defaultBranch); err == nil || owners != nil {
				t.Fatalf("must not return stale ownership on lookup failure: %v, %v", owners, err)
			}
		})
	}
}

func TestTeamAdvisoryApprovers(t *testing.T) {
	files := map[string][]byte{
		"OWNERS": []byte("approvers: [alice]\nadvisory_approvers: ['@org/team']\nrequired_reviewers: [alice]\n"),
	}
	client, cleanup, err := getTestClient(files, false, false, false, false, nil, nil, nil, nil, localgit.NewV2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	client.ghc.(*fakeGitHubClient).teams = map[string][]github.TeamMember{"org/team": {{Login: "bob"}}}
	owners, err := client.LoadRepoOwners("org", "repo", defaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	if !owners.Approvers("file.go").Set().Equal(sets.New("alice", "bob")) {
		t.Errorf("team member must have advisory approval authority: %v", owners.AllApprovers())
	}
	if !owners.LeafApprovers("file.go").Equal(sets.New("alice")) {
		t.Errorf("advisory team must not be auto-assigned: %v", owners.LeafApprovers("file.go"))
	}
	if !owners.RequiredReviewers("file.go").Equal(sets.New("alice")) {
		t.Errorf("individual required reviewers changed: %v", owners.RequiredReviewers("file.go"))
	}
}

func TestTeamsInRequiredReviewers(t *testing.T) {
	for _, reference := range []string{"@org/team", "maintainers"} {
		files := map[string][]byte{
			"OWNERS":         []byte("approvers: [alice]\nrequired_reviewers: [alice, '" + reference + "']\n"),
			"OWNERS_ALIASES": []byte("aliases:\n  maintainers: ['@org/team']\n"),
		}
		client, cleanup, err := getTestClient(files, false, true, false, false, nil, nil, nil, nil, localgit.NewV2)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
		ghc := client.ghc.(*fakeGitHubClient)
		ghc.teams = map[string][]github.TeamMember{"org/team": {{Login: "alice"}, {Login: "bob"}, {Login: "carol"}}}
		owners, err := client.LoadRepoOwners("org", "repo", defaultBranch)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := owners.RequiredReviewers("file.go"), sets.New("alice", "bob", "carol"); !got.Equal(want) {
			t.Errorf("required reviewers for %q: got %v, want all members %v", reference, got, want)
		}
		if ghc.teamCalls != 1 {
			t.Errorf("got %d membership lookups, want 1", ghc.teamCalls)
		}
		ghc.teams["org/team"] = []github.TeamMember{{Login: "dave"}}
		updated, err := client.LoadRepoOwners("org", "repo", defaultBranch)
		if err != nil {
			t.Fatal(err)
		}
		if !updated.RequiredReviewers("file.go").Equal(sets.New("alice", "dave")) {
			t.Errorf("required reviewers did not refresh: %v", updated.RequiredReviewers("file.go"))
		}
		if !owners.RequiredReviewers("file.go").Equal(sets.New("alice", "bob", "carol")) {
			t.Error("refresh mutated the previous required-reviewer snapshot")
		}
	}
}
