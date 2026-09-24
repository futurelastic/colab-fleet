#!/bin/sh
# Build ONE optional delivery module from its source, if - and only if - the
# user running this script can get at that source. colab-fleet #185.
#
#   scripts/fetch-module.sh NAME SOURCE OUT
#
# WHY THIS EXISTS
#
# An external delivery module is optional and lives in somebody else's
# repository, which not every installing user can read. "Could this user fetch
# it?" is the whole access test: this script asks the Go toolchain to fetch and
# build the module with the CALLING user's own credentials and configuration
# (GOPRIVATE, GOPROXY, netrc, git and ssh config, ...), and a user who cannot
# reach it simply ends up with a machine that has no such module. That is a
# first-class, supported state - the built-in terminal module always exists and
# is the fallback - not an error, so this script never reports one.
#
# THE CONTRACT
#
#   - It ALWAYS exits 0, and prints NOTHING on stdout or stderr, whether the
#     fetch worked or not. The source is never echoed: a source can carry a
#     credential in its shape, and "silently skipped" is only true if nothing
#     spoke. (The caller decides whether to say something afterwards, by
#     looking at whether OUT exists.)
#   - On success OUT is an executable regular file (mode 0755) built for
#     $GOOS/$GOARCH - the caller's GOOS/GOARCH if set, otherwise the
#     toolchain's own. The binary is built in a scratch directory beside OUT and
#     renamed into place, so OUT is never observable half-written.
#   - On any failure this script creates no OUT, and leaves no scratch files
#     behind. A file already at OUT is never modified or removed by a failed
#     fetch; the only thing that touches OUT is the final rename of a fully
#     built, executable binary.
#
# SOURCE FORMS, recognised by shape
#
#   <go-package-path>@<version>   the first path element has a dot, e.g.
#                                 example.invalid/mod/cmd/tool@v1.2.3. Built in
#                                 a throwaway Go module (`go get`, then
#                                 `go build`), so nothing is read from, or
#                                 written to, the current directory.
#   /absolute/directory           a directory holding a Go main package
#                                 (`go build .` inside it).
#   anything else                 skipped. A relative path is deliberately not a
#                                 form: what it names depends on the caller's
#                                 working directory.
#
# NAME must be 1-32 characters of a-z, 0-9 and '-', starting with a letter, and
# not one of the names the service reserves (auto, terminal, inbox, module);
# anything else is skipped. NAME only decides whether to build - the caller
# passes the final path in OUT and OUT's own name is not inspected.
#
# BOUNDS
#
# The whole fetch-and-build is bounded to 300 seconds when a `timeout` (or
# `gtimeout`) is on PATH, and unbounded otherwise. Prompting is switched off
# (stdin is /dev/null and GIT_TERMINAL_PROMPT defaults to 0): a source this
# user cannot authenticate to must fail fast, not ask, or "skipped silently"
# would mean "hangs the deploy".
#
# Deliberately absent: hostnames, module names and source locations. Those are
# operational facts, and this repository is public.

set -u

# Everything below is forbidden from speaking; see THE CONTRACT.
exec </dev/null >/dev/null 2>&1

NAME=${1-}
SOURCE=${2-}
OUT=${3-}

STAGE=""
WORK=""

cleanup() {
	if [ -n "$STAGE" ]; then rm -rf "$STAGE"; fi
	if [ -n "$WORK" ]; then rm -rf "$WORK"; fi
	return 0
}

# `exit 0` in the EXIT trap is the guarantee behind "always exits 0": it also
# overrides the status of a shell that stopped for any other reason.
trap 'cleanup; exit 0' EXIT
trap 'exit 0' HUP INT TERM

# --- bound the whole run, by re-running this script under `timeout` ----------
#
# A shell function cannot be handed to `timeout`, so the bound is applied by
# running this same file again, marked as the inner run. Backgrounded and
# waited on so an interrupt of this script reaches the child instead of
# leaving it running for the rest of its 300 seconds.
if [ -z "${FLEET_FETCH_MODULE_INNER:-}" ]; then
	TMO=""
	if command -v timeout >/dev/null 2>&1; then
		TMO=timeout
	elif command -v gtimeout >/dev/null 2>&1; then
		TMO=gtimeout
	fi
	if [ -n "$TMO" ] && [ -f "$0" ]; then
		FLEET_FETCH_MODULE_INNER=1 "$TMO" 300 sh "$0" "$NAME" "$SOURCE" "$OUT" &
		CHILD=$!
		trap 'kill "$CHILD" 2>/dev/null; exit 0' HUP INT TERM
		wait "$CHILD"
		exit 0
	fi
fi

# Explicit character sets, not ranges: a range like a-z means different things
# in different locales.
LOWER=abcdefghijklmnopqrstuvwxyz
UPPER=ABCDEFGHIJKLMNOPQRSTUVWXYZ
DIGITS=0123456789

valid_name() {
	case "$NAME" in
	'' | auto | terminal | inbox | module) return 1 ;;
	[!$LOWER]* | *[!$LOWER$DIGITS-]*) return 1 ;;
	esac
	[ "${#NAME}" -le 32 ]
}

# A go package path with a version, e.g. example.invalid/mod/cmd/tool@v1.2.3.
build_pkg() {
	case "$SOURCE" in
	*[!$LOWER$UPPER$DIGITS._~/@+-]*) return 1 ;;
	esac
	pkg=${SOURCE%@*}
	ver=${SOURCE##*@}
	[ -n "$pkg" ] && [ -n "$ver" ] || return 1
	# One '@' only; and never something `go get` would read as a flag or a
	# local path.
	case "$pkg" in
	*@* | -* | /* | */) return 1 ;;
	esac
	case "${pkg%%/*}" in
	*.*) ;;
	*) return 1 ;;
	esac

	WORK=$(mktemp -d "${TMPDIR:-/tmp}/fleet-module-fetch.XXXXXX") || return 1
	(
		cd "$WORK" || exit 1
		# A workspace file in the caller's environment must not leak into the
		# throwaway module.
		GOWORK=off
		export GOWORK
		go mod init fleet-module-fetch || exit 1
		go get "$pkg@$ver" || exit 1
		[ "$(go list -f '{{.Name}}' "$pkg")" = main ] || exit 1
		go build -buildvcs=false -o "$BIN" "$pkg"
	)
}

# An absolute directory holding a Go main package.
build_dir() {
	[ -d "$SOURCE" ] || return 1
	(
		cd "$SOURCE" || exit 1
		[ "$(go list -f '{{.Name}}' .)" = main ] || exit 1
		go build -buildvcs=false -o "$BIN" .
	)
}

fetch() {
	valid_name || return 1

	case "$OUT" in
	'' | *'
'*) return 1 ;;
	esac
	if [ -d "$OUT" ]; then return 1; fi
	case "$OUT" in
	/*) ;;
	*) OUT="$(pwd)/$OUT" ;;
	esac
	OUT_DIR=$(dirname -- "$OUT") || return 1
	[ -d "$OUT_DIR" ] || return 1

	command -v go >/dev/null 2>&1 || return 1
	: "${GOOS:=$(go env GOOS)}" "${GOARCH:=$(go env GOARCH)}"
	[ -n "$GOOS" ] && [ -n "$GOARCH" ] || return 1
	export GOOS GOARCH
	: "${GIT_TERMINAL_PROMPT:=0}"
	export GIT_TERMINAL_PROMPT

	# Scratch space beside OUT, so the final mv is a same-filesystem rename
	# (atomic). A directory rather than a file: `go build -o` refuses to
	# overwrite a file that is not already a binary.
	STAGE=$(mktemp -d "$OUT_DIR/.fetch-module.XXXXXX") || return 1
	BIN="$STAGE/module"

	case "$SOURCE" in
	/*) build_dir || return 1 ;;
	*@*) build_pkg || return 1 ;;
	*) return 1 ;;
	esac

	[ -f "$BIN" ] && [ -s "$BIN" ] || return 1
	chmod 0755 "$BIN" || return 1
	mv -f "$BIN" "$OUT"
}

fetch
exit 0
