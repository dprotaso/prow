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
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/prow/pkg/github"
)

// TeamMembershipClient is the GitHub API needed to resolve team references.
// Using the normal GitHub client allows ghproxy to cache and revalidate requests.
type TeamMembershipClient interface {
	ListTeamMembersBySlug(org, teamSlug, role string) ([]github.TeamMember, error)
}

// ErrInvalidTeamReference identifies invalid syntax or a disallowed organization,
// as distinct from a temporary failure to read membership from GitHub.
var ErrInvalidTeamReference = errors.New("invalid team reference")

var teamReferenceRE = regexp.MustCompile(`^@([a-z0-9][a-z0-9-]*)/([a-z0-9][a-z0-9_-]*)$`)

// TeamResolver expands @org/team-slug references into individual logins. It is
// scoped to one ownership load: memberships must not be cached by repository SHA.
// A resolver is not safe for concurrent use.
type TeamResolver struct {
	org     string
	enabled bool
	client  TeamMembershipClient
	members map[string]sets.Set[string]
}

// NewTeamResolver constructs a resolver for teams in the repository's organization.
// Disabled resolvers reject team references without querying GitHub.
func NewTeamResolver(org string, client TeamMembershipClient, enabled bool) *TeamResolver {
	return &TeamResolver{org: strings.ToLower(org), enabled: enabled, client: client, members: map[string]sets.Set[string]{}}
}

// Resolve expands team references, returning a new set without modifying owners.
// Repeated references share a membership snapshot within this resolver. Errors
// are propagated, never replaced with empty or stale memberships.
func (r *TeamResolver) Resolve(owners sets.Set[string]) (sets.Set[string], error) {
	result := sets.New[string]()
	for _, owner := range sets.List(owners) {
		if !strings.HasPrefix(owner, "@") || !strings.Contains(owner, "/") {
			result.Insert(owner)
			continue
		}
		if !r.enabled {
			return nil, fmt.Errorf("%w %q: GitHub teams are disabled; set owners.enable_github_teams to true in plugins.yaml", ErrInvalidTeamReference, owner)
		}
		ref := strings.ToLower(owner)
		match := teamReferenceRE.FindStringSubmatch(ref)
		if match == nil {
			return nil, fmt.Errorf("%w %q: expected @org/team-slug", ErrInvalidTeamReference, owner)
		}
		if match[1] != r.org {
			return nil, fmt.Errorf("%w %q: must belong to repository organization %q", ErrInvalidTeamReference, owner, r.org)
		}
		members, ok := r.members[ref]
		if !ok {
			users, err := r.client.ListTeamMembersBySlug(match[1], match[2], github.RoleAll)
			if err != nil {
				return nil, fmt.Errorf("resolve team %q: %w", owner, err)
			}
			members = sets.New[string]()
			for _, user := range users {
				members.Insert(github.NormLogin(user.Login))
			}
			r.members[ref] = members
		}
		result = result.Union(members)
	}
	return result, nil
}

// resolveTeams returns a new ownership snapshot, leaving the SHA-keyed cache
// untouched. Alias expansion has already happened while parsing ownership maps.
func (o *RepoOwners) resolveTeams(resolver *TeamResolver) (*RepoOwners, error) {
	resolve := func(source map[string]map[*regexp.Regexp]sets.Set[string]) (map[string]map[*regexp.Regexp]sets.Set[string], error) {
		if source == nil {
			return nil, nil
		}
		result := make(map[string]map[*regexp.Regexp]sets.Set[string], len(source))
		for path, entries := range source {
			result[path] = make(map[*regexp.Regexp]sets.Set[string], len(entries))
			for re, owners := range entries {
				resolved, err := resolver.Resolve(owners)
				if err != nil {
					return nil, fmt.Errorf("owners for %q: %w", path, err)
				}
				result[path][re] = resolved
			}
		}
		return result, nil
	}
	result := *o
	var err error
	if result.approvers, err = resolve(o.approvers); err != nil {
		return nil, err
	}
	if result.reviewers, err = resolve(o.reviewers); err != nil {
		return nil, err
	}
	if result.requiredReviewers, err = resolve(o.requiredReviewers); err != nil {
		return nil, err
	}
	if result.advisoryApprovers, err = resolve(o.advisoryApprovers); err != nil {
		return nil, err
	}
	if o.RepoAliases != nil {
		result.RepoAliases = make(RepoAliases, len(o.RepoAliases))
	}
	for alias, owners := range o.RepoAliases {
		if result.RepoAliases[alias], err = resolver.Resolve(owners); err != nil {
			return nil, fmt.Errorf("alias %q: %w", alias, err)
		}
	}
	return &result, nil
}
