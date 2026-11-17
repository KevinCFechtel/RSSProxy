"""Validate release tags before granting them stable image aliases."""

import os
import re
import sys

NUMBER = r"(?:0|[1-9][0-9]*)"
IDENTIFIER = rf"(?:{NUMBER}|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)"
SEMVER = re.compile(
    rf"v(?P<version>{NUMBER}\.{NUMBER}\.{NUMBER})"
    rf"(?:-(?P<prerelease>{IDENTIFIER}(?:\.{IDENTIFIER})*))?"
    r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?"
)


def validate(tag):
    match = SEMVER.fullmatch(tag)
    if match is None:
        raise ValueError("Release tags must be semantic versions, for example v1.2.3")
    return match.group("prerelease") is None


def main():
    tag = os.environ.get("RELEASE_TAG", "")
    try:
        stable = validate(tag)
    except ValueError as error:
        print(f"::error::{error}", file=sys.stderr)
        return 1
    output = f"stable={str(stable).lower()}\n"
    if path := os.environ.get("GITHUB_OUTPUT"):
        with open(path, "a", encoding="utf-8") as handle:
            handle.write(output)
    print(f"Validated {tag} ({'stable' if stable else 'prerelease'})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
