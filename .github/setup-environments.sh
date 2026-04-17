#!/usr/bin/env bash
# Create the `npm` and `pypi` GitHub deployment environments referenced by
# .github/workflows/release-{npm,pypi}.yml.
#
# Each environment:
#   - is restricted to tags matching its namespace (npm-v* / pypi-v*) so the
#     environment can only be exercised by the intended release tag
#
# The environment name is also part of the OIDC claim used by npm + PyPI
# trusted publishing, so the trusted-publisher entries on each registry must
# specify the same environment name (`npm` and `pypi` respectively).
#
# Note: required-reviewer protection rules need a paid GitHub plan on private
# repos (Team / Enterprise). Add reviewers via the UI once the repo is public
# or the org upgrades:
#   Settings → Environments → npm → "Required reviewers"
#
# Idempotent — re-running updates the environments in place.
#
# Usage:
#   ./.github/setup-environments.sh

set -euo pipefail

REPO="deepnoodle-ai/mobius"

create_env() {
  local name="$1"
  local tag_pattern="$2"

  echo "==> Creating environment: $name (tag pattern: $tag_pattern)"

  gh api -X PUT "repos/$REPO/environments/$name" --input - <<EOF
{
  "deployment_branch_policy": {
    "protected_branches": false,
    "custom_branch_policies": true
  }
}
EOF

  # Wipe any existing tag policies so re-runs don't accumulate duplicates.
  existing_ids=$(gh api "repos/$REPO/environments/$name/deployment-branch-policies" \
    --jq '.branch_policies[].id' 2>/dev/null || true)
  for id in $existing_ids; do
    gh api -X DELETE "repos/$REPO/environments/$name/deployment-branch-policies/$id" >/dev/null
  done

  gh api -X POST "repos/$REPO/environments/$name/deployment-branch-policies" \
    -f name="$tag_pattern" -f type='tag' >/dev/null
}

create_env npm  'npm-v*'
create_env pypi 'pypi-v*'

echo
echo "Done. Verify with:"
echo "  gh api repos/$REPO/environments --jq '.environments[] | {name, protection_rules}'"
