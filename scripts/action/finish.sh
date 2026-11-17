#!/usr/bin/env bash
set -euo pipefail

code="${ZYG_EXIT_CODE:?}"
if [[ ! "$code" =~ ^[0-9]+$ ]]; then
  echo "::error::invalid zyg exit code '$code'"
  exit 3
fi
if [[ "$code" -ne 0 ]]; then
  echo "::error title=zyg verify failed::zyg verify exited with code $code. See the job summary for the diverging steps."
fi
exit "$code"
