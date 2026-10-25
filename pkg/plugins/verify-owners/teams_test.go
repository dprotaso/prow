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

package verifyowners

import (
	"errors"
	"testing"

	"github.com/sirupsen/logrus"

	"sigs.k8s.io/prow/pkg/git/localgit"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/github/fakegithub"
	"sigs.k8s.io/prow/pkg/labels"
	"sigs.k8s.io/prow/pkg/plugins"
	"sigs.k8s.io/prow/pkg/plugins/ownersconfig"
)

type teamGitHubClient struct {
	*fakegithub.FakeClient
	lookupErr error
	calls     int
}

func (c *teamGitHubClient) ListTeamMembersBySlug(org, slug, role string) ([]github.TeamMember, error) {
	c.calls++
	if c.lookupErr != nil {
		return nil, c.lookupErr
	}
	return []github.TeamMember{{Login: "alice"}}, nil
}

func TestHandleTeams(t *testing.T) {
	for _, tc := range []struct {
		name        string
		owners      string
		aliases     string
		aliasOnly   bool
		skipTrusted bool
		lookupErr   error
		wantInvalid bool
		wantError   bool
		wantCalls   int
	}{
		{name: "direct team", owners: "approvers: ['@org/leads']\nreviewers: ['@org/leads']", wantCalls: 1},
		{name: "team alias", owners: "approvers: [maintainers]", aliases: "aliases:\n  maintainers: ['@org/leads']", wantCalls: 1},
		{name: "team alias without trusted checks", owners: "approvers: [maintainers]", aliases: "aliases:\n  maintainers: ['@org/leads']", skipTrusted: true, wantCalls: 1},
		{name: "cross organization", owners: "approvers: ['@other/leads']", wantInvalid: true},
		{name: "cross organization without trusted checks", owners: "approvers: ['@other/leads']", skipTrusted: true, wantInvalid: true},
		{name: "invalid slug", owners: "approvers: ['@org/']", wantInvalid: true},
		{name: "invalid alias team", owners: "approvers: [maintainers]", aliases: "aliases:\n  maintainers: ['@other/leads']", wantInvalid: true},
		{name: "required team", owners: "approvers: [alice]\nrequired_reviewers: ['@org/leads']", wantCalls: 1},
		{name: "required team in filter", owners: "filters:\n  '.*':\n    approvers: [alice]\n    required_reviewers: ['@org/leads']", wantCalls: 1},
		{name: "required team via alias", owners: "approvers: [alice]\nrequired_reviewers: [maintainers]", aliases: "aliases:\n  maintainers: ['@org/leads']", wantCalls: 1},
		{name: "alias-only change with required team", owners: "approvers: [alice]\nrequired_reviewers: [maintainers]", aliases: "aliases:\n  maintainers: ['@org/leads']", aliasOnly: true, wantCalls: 1},
		{name: "required team without trusted checks", owners: "approvers: [alice]\nrequired_reviewers: ['@org/leads']", skipTrusted: true, wantCalls: 1},
		{name: "cross organization required team", owners: "approvers: [alice]\nrequired_reviewers: ['@other/leads']", wantInvalid: true},
		{name: "required team not found", owners: "approvers: [alice]\nrequired_reviewers: ['@org/leads']", lookupErr: github.NewNotFound(), wantInvalid: true, wantCalls: 1},
		{name: "required team lookup failure", owners: "approvers: [alice]\nrequired_reviewers: ['@org/leads']", lookupErr: errors.New("unavailable"), wantError: true, wantCalls: 1},
		{name: "not found", owners: "approvers: ['@org/leads']", lookupErr: github.NewNotFound(), wantInvalid: true, wantCalls: 1},
		{name: "temporary failure", owners: "approvers: ['@org/leads']", lookupErr: errors.New("unavailable"), wantError: true, wantCalls: 1},
		{name: "temporary alias failure", owners: "approvers: [maintainers]", aliases: "aliases:\n  maintainers: ['@org/leads']", lookupErr: errors.New("unavailable"), wantError: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lg, gc, err := localgit.NewV2()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { gc.Clean(); lg.Clean() })
			if err := lg.MakeFakeRepo("org", "repo"); err != nil {
				t.Fatal(err)
			}
			base := "approvers: [alice]\n"
			if tc.aliasOnly {
				base = tc.owners
			}
			if err := lg.AddCommit("org", "repo", map[string][]byte{"OWNERS": []byte(base)}); err != nil {
				t.Fatal(err)
			}
			baseSHA, err := lg.RevParse("org", "repo", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if err := lg.CheckoutNewBranch("org", "repo", "pr"); err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{}
			changes := map[string]string{}
			if !tc.aliasOnly {
				files["OWNERS"] = []byte(tc.owners)
				changes["OWNERS"] = "@@ -1 +1 @@\n-approvers: [alice]\n+" + tc.owners
			}
			if tc.aliases != "" {
				files["OWNERS_ALIASES"] = []byte(tc.aliases)
				changes["OWNERS_ALIASES"] = "+" + tc.aliases
			}
			if err := lg.AddCommit("org", "repo", files); err != nil {
				t.Fatal(err)
			}
			sha, err := lg.RevParse("org", "repo", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			ghc := &teamGitHubClient{FakeClient: newFakeGitHubClient(changes, nil, 1), lookupErr: tc.lookupErr}
			pr := &github.PullRequest{
				Base: github.PullRequestBranch{Ref: defaultBranch, SHA: baseSHA},
				Head: github.PullRequestBranch{SHA: sha},
			}
			err = handle(ghc, gc, makeFakeRepoOwnersClient(), logrus.NewEntry(logrus.New()), pr,
				info{org: "org", repo: "repo", repoFullName: "org/repo", number: 1}, nil,
				plugins.Trigger{}, tc.skipTrusted, &fakePruner{}, ownersconfig.FakeResolver)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error: %t", err, tc.wantError)
			}
			if got := IssueLabelsContain(ghc.IssueLabelsAdded, labels.InvalidOwners); got != tc.wantInvalid {
				t.Errorf("invalid-owners label = %t, want %t", got, tc.wantInvalid)
			}
			if ghc.calls != tc.wantCalls {
				t.Errorf("got %d team lookups, want %d", ghc.calls, tc.wantCalls)
			}
		})
	}
}
