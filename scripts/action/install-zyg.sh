#!/usr/bin/env bash
set -euo pipefail

: "${ZYG_VERSION:?}" "${RUNNER_TEMP:?}" "${GITHUB_PATH:?}"
bin_dir="$RUNNER_TEMP/zyg-bin"
mkdir -p "$bin_dir"

if [[ "$ZYG_VERSION" == "source" ]]; then
  : "${ACTION_PATH:?}"
  (cd "$ACTION_PATH" && go build -trimpath -o "$bin_dir/zyg" ./cmd/zyg)
else
  if [[ ! "$ZYG_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
    echo "::error title=Invalid zyg-version::zyg-version must be 'source' or a release tag such as v0.3.0 (got '$ZYG_VERSION')."
    exit 1
  fi

  case "${RUNNER_OS:-}" in
    Linux) os=Linux ;;
    macOS) os=Darwin ;;
    *)
      echo "::error::unsupported runner OS '${RUNNER_OS:-}'"
      exit 1
      ;;
  esac
  case "${RUNNER_ARCH:-}" in
    X64) arch=x86_64 ;;
    ARM64) arch=arm64 ;;
    *)
      echo "::error::unsupported runner architecture '${RUNNER_ARCH:-}'"
      exit 1
      ;;
  esac

  archive="zygote_${os}_${arch}.tar.gz"
  base="https://github.com/mindfire-test/zygote/releases/download/${ZYG_VERSION}"
  work="$(mktemp -d "$RUNNER_TEMP/zyg-download.XXXXXX")"
  curl -fsSL --retry 3 -o "$work/$archive" "$base/$archive"
  curl -fsSL --retry 3 -o "$work/checksums.txt" "$base/checksums.txt"

  expected="$(awk -v f="$archive" '$2 == f { print $1 }' "$work/checksums.txt")"
  if [[ -z "$expected" ]]; then
    echo "::error::$archive is not listed in checksums.txt for $ZYG_VERSION"
    exit 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$work/$archive" | awk '{ print $1 }')"
  else
    actual="$(shasum -a 256 "$work/$archive" | awk '{ print $1 }')"
  fi
  if [[ "$actual" != "$expected" ]]; then
    echo "::error::checksum mismatch for $archive (expected $expected, got $actual)"
    exit 1
  fi

  tar -xzf "$work/$archive" -C "$work" zyg
  install -m 0755 "$work/zyg" "$bin_dir/zyg"
  rm -rf "$work"
fi

echo "$bin_dir" >>"$GITHUB_PATH"
"$bin_dir/zyg" --help >/dev/null
