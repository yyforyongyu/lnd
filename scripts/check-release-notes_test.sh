#!/usr/bin/env bash
# Run with bash scripts/check-release-notes_test.sh. Real Git additions keep
# these checks on the public fragment validator rather than a copied matcher.

set -euo pipefail

release_script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
release_check_script="$release_script_dir/check-release-notes.sh"
release_test_dir="$(mktemp -d)"
trap 'rm -rf "$release_test_dir"' EXIT

# checkRepositoryLink validates one repository/link pair against a newly
# committed fragment, so the real added-file link check runs for every case.
checkRepositoryLink() {
    local repository="$1" linked_repository="$2" expected_status="$3"
    local fixture notes base output actual_status=0

    # Arrange an isolated Git history with valid headings, then commit the
    # fragment separately so the validator sees an addition against its base.
    fixture="$(mktemp -d "$release_test_dir/case.XXXXXX")"
    notes="$fixture/docs/release-notes"
    mkdir -p "$notes/unreleased"
    printf '# Improvements\n' > "$notes/release-notes-template.md"
    cp "$notes/release-notes-template.md" "$notes/release-notes-0.0.0.md"
    git init --quiet "$fixture"
    git -C "$fixture" config user.name 'Release Note Test'
    git -C "$fixture" config user.email 'release-notes@example.invalid'
    git -C "$fixture" config commit.gpgsign false
    git -C "$fixture" config core.hooksPath /dev/null
    git -C "$fixture" add docs
    git -C "$fixture" commit --quiet -m 'Fixture headings'
    base="$(git -C "$fixture" rev-parse HEAD)"
    printf '# Improvements\n\n* [Change](https://github.com/%s/pull/77).\n' \
        "$linked_repository" > "$notes/unreleased/77.md"
    git -C "$fixture" add docs
    git -C "$fixture" commit --quiet -m 'Fixture fragment'

    # Act through the unchanged command interface. An empty repository case
    # removes the GitHub variable to exercise the existing local-run default.
    output=$(
        cd "$fixture"
        if [[ -n "$repository" ]]; then
            export GITHUB_REPOSITORY="$repository"
        else
            unset GITHUB_REPOSITORY
        fi
        GITHUB_REF=refs/pull/77/merge RELEASE_NOTES_BASE="$base" \
            bash "$release_check_script"
    ) || actual_status=$?

    # Assert the public exit status. A rejection must specifically identify
    # the fragment link, so a fixture or unrelated validation error cannot pass.
    if [[ "$actual_status" -ne "$expected_status" ]]; then
        printf 'Unexpected status %s (wanted %s) for %s -> %s\n%s\n' \
            "$actual_status" "$expected_status" \
            "$repository" "$linked_repository" "$output" >&2
        exit 1
    fi
    if [[ "$expected_status" -ne 0 ]] &&
        ! grep -qF 'fragment must link to this PR' <<< "$output"; then

        printf 'Expected link rejection, got:\n%s\n' "$output" >&2
        exit 1
    fi
    printf 'PASS repository=%s link=%s\n' "$repository" "$linked_repository"
}

# A configured fork accepts its own link even when its name contains a period.
checkRepositoryLink 'owner/lnd.x' 'owner/lnd.x' 0

# A period denotes the repository's literal name, not a wildcard matching X.
checkRepositoryLink 'owner/lnd.x' 'owner/lndXx' 1

# Local invocation without GitHub context retains the upstream repository.
checkRepositoryLink '' 'lightningnetwork/lnd' 0
