#!/usr/bin/env bash
set -euo pipefail

: "${ZYG_CORPUS:?}" "${ZYG_AGENT:?}" "${RUNNER_TEMP:?}" "${GITHUB_OUTPUT:?}"

out_dir="$(mktemp -d "$RUNNER_TEMP/zyg-verify.XXXXXX")"
report="$out_dir/report.json"
markdown="$out_dir/report.md"

secret_file() {
  local value="$1" path="$2" line
  while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ -n "$line" ]]; then
      echo "::add-mask::$line"
    fi
  done <<<"$value"
  (umask 077 && printf '%s\n' "$value" >"$path")
  chmod 600 "$path"
}

if [[ -n "${INPUT_PASSPHRASE:-}" ]]; then
  secret_file "$INPUT_PASSPHRASE" "$out_dir/passphrase"
  export ZYG_PASSPHRASE_FILE="$out_dir/passphrase"
fi
if [[ -n "${INPUT_IDENTITY:-}" ]]; then
  secret_file "$INPUT_IDENTITY" "$out_dir/identity"
  export ZYG_IDENTITY_FILE="$out_dir/identity"
fi
unset INPUT_PASSPHRASE INPUT_IDENTITY

args=(verify --json --markdown "$markdown" --corpus "$ZYG_CORPUS" --agent "$ZYG_AGENT")
if [[ -n "${ZYG_APPROVALS:-}" ]]; then
  args+=(--approvals "$ZYG_APPROVALS")
fi
if [[ -n "${ZYG_TIMEOUT:-}" ]]; then
  args+=(--timeout "$ZYG_TIMEOUT")
fi

set +e
zyg "${args[@]}" >"$report"
code=$?
set -e

if [[ ! -s "$markdown" ]]; then
  {
    echo "<!-- zyg-verify corpus=${ZYG_CORPUS//--/-} -->"
    echo "## zyg verify: error (exit $code)"
    echo
    echo "zyg did not produce a report; see the job log."
  } >"$markdown"
fi

echo "::group::zyg verify report (exit $code)"
cat "$report"
echo "::endgroup::"

if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  cat "$markdown" >>"$GITHUB_STEP_SUMMARY"
fi

{
  echo "exit-code=$code"
  echo "report=$report"
  echo "markdown=$markdown"
} >>"$GITHUB_OUTPUT"
