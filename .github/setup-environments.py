#!/usr/bin/env python3
"""Create the GitHub deployment environments used by release workflows."""

from __future__ import annotations

import json
import subprocess
import sys

REPO = "deepnoodle-ai/mobius"
ENVIRONMENTS = (
    ("npm", "npm-v*"),
    ("pypi", "pypi-v*"),
    ("cli", "v*"),
)


def run(cmd: list[str], *, input_text: str | None = None, capture: bool = False, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        cmd,
        input=input_text,
        text=True,
        capture_output=capture,
        check=check,
    )


def create_environment(name: str, tag_pattern: str) -> None:
    print(f"==> Creating environment: {name} (tag pattern: {tag_pattern})")
    payload = json.dumps(
        {
            "deployment_branch_policy": {
                "protected_branches": False,
                "custom_branch_policies": True,
            }
        }
    )
    run(["gh", "api", "-X", "PUT", f"repos/{REPO}/environments/{name}", "--input", "-"], input_text=payload)

    result = run(
        ["gh", "api", f"repos/{REPO}/environments/{name}/deployment-branch-policies"],
        capture=True,
        check=False,
    )
    if result.returncode == 0 and result.stdout.strip():
        data = json.loads(result.stdout)
        for policy in data.get("branch_policies", []):
            policy_id = policy.get("id")
            if policy_id is None:
                continue
            run(["gh", "api", "-X", "DELETE", f"repos/{REPO}/environments/{name}/deployment-branch-policies/{policy_id}"])

    run(
        [
            "gh",
            "api",
            "-X",
            "POST",
            f"repos/{REPO}/environments/{name}/deployment-branch-policies",
            "-f",
            f"name={tag_pattern}",
            "-f",
            "type=tag",
        ]
    )


def main() -> int:
    for name, pattern in ENVIRONMENTS:
        create_environment(name, pattern)

    print("\nDone. Verify with:")
    print(f"  gh api repos/{REPO}/environments --jq '.environments[] | {{name, protection_rules}}'")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
