#!/usr/bin/env bash
#
# build-release.sh -- produce the Cortex Phase 1 release binary.
#
# Acceptance criteria from cortex-4kq.10 (release-build-darwin-arm64):
#   1. `make release` produces bin/cortex whose `file` output includes
#      "Mach-O 64-bit executable arm64".
#   2. `bin/cortex version` prints the stamped version tag.
#   3. Two clean builds from the same commit are byte-identical.
#   4. The final binary has zero third-party dynamic library dependencies.
#
# Reproducibility notes:
#   - CGO_ENABLED=0 removes all non-stdlib dynamic linkage.
#   - -trimpath strips absolute working-directory prefixes.
#   - -buildvcs=false drops VCS state from the binary.
#   - -ldflags "-buildid=" clears the Go build ID so independent clean
#     builds of the same source tree yield identical bytes.
#   - -ldflags "-s -w" strips DWARF + symbol tables, also removing a
#     common source of non-determinism.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/.." && pwd)"
cd "${root}"

BINARY="${BINARY:-bin/cortex}"
MODULE="${MODULE:-github.com/nixlim/cortex}"
VERSION_PKG="${MODULE}/internal/version"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
GOOS_RELEASE="${GOOS_RELEASE:-darwin}"
GOARCH_RELEASE="${GOARCH_RELEASE:-arm64}"

mkdir -p "$(dirname "${BINARY}")"

CGO_ENABLED=0 \
GOOS="${GOOS_RELEASE}" \
GOARCH="${GOARCH_RELEASE}" \
go build \
    -trimpath \
    -buildvcs=false \
    -ldflags "-s -w -buildid= -X ${VERSION_PKG}.Version=${VERSION}" \
    -o "${BINARY}" \
    ./cmd/cortex

echo "built: ${BINARY}"
echo "version: ${VERSION}"
echo "target: ${GOOS_RELEASE}/${GOARCH_RELEASE}"

# Post-build verification (best-effort — only runs the checks that can be
# performed on the build host). Any failure here exits non-zero.

if command -v file >/dev/null 2>&1; then
    file_out="$(file "${BINARY}")"
    echo "file: ${file_out}"
    if [ "${GOOS_RELEASE}" = "darwin" ] && [ "${GOARCH_RELEASE}" = "arm64" ]; then
        case "${file_out}" in
            *"Mach-O 64-bit executable arm64"*) ;;
            *) echo "FAIL: expected Mach-O 64-bit executable arm64 in file output" >&2; exit 1 ;;
        esac
    fi
fi

# Version stamping is only verifiable when the host can execute the binary.
host_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
host_arch="$(uname -m)"
case "${host_arch}" in
    arm64|aarch64) host_arch="arm64" ;;
    x86_64|amd64)  host_arch="amd64" ;;
esac
if [ "${host_os}" = "${GOOS_RELEASE}" ] && [ "${host_arch}" = "${GOARCH_RELEASE}" ]; then
    got_version="$("${BINARY}" version)"
    if [ "${got_version}" != "${VERSION}" ]; then
        echo "FAIL: binary reported version '${got_version}', expected '${VERSION}'" >&2
        exit 1
    fi
    echo "verified: ${BINARY} version == ${VERSION}"

    # Acceptance 4: no third-party dynamic dependencies on darwin.
    # Apple's /usr/lib/libSystem.B.dylib is the OS C library, not third-party.
    if [ "${GOOS_RELEASE}" = "darwin" ] && command -v otool >/dev/null 2>&1; then
        deps="$(otool -L "${BINARY}" | tail -n +2 | awk '{print $1}')"
        bad=""
        while IFS= read -r dep; do
            [ -z "${dep}" ] && continue
            case "${dep}" in
                /usr/lib/*|/System/*) ;;
                *) bad="${bad}${dep}"$'\n' ;;
            esac
        done <<< "${deps}"
        if [ -n "${bad}" ]; then
            echo "FAIL: binary has non-system dynamic dependencies:" >&2
            printf '%s' "${bad}" >&2
            exit 1
        fi
        echo "verified: no third-party dynamic dependencies"
    fi
fi
