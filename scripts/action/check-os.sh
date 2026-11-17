#!/usr/bin/env bash
set -euo pipefail

case "${RUNNER_OS:-}" in
  Linux | macOS) ;;
  *)
    echo "::error title=Unsupported runner::mindfire-test/zygote supports Linux and macOS runners only (got '${RUNNER_OS:-unknown}'). Use runs-on: ubuntu-latest or macos-latest."
    exit 1
    ;;
esac
