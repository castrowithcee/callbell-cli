#!/bin/sh
set -eu

version=${1:-}
output=${2:-dist}

if ! printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'; then
	printf 'release: version must be a SemVer tag such as v0.1.0 or v0.1.0-rc.1\n' >&2
	exit 2
fi

if [ -d "$output" ] && [ -n "$(find "$output" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
	printf 'release: output directory is not empty: %s\n' "$output" >&2
	exit 2
fi

mkdir -p "$output"
output=$(cd "$output" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM

if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
	SOURCE_DATE_EPOCH=$(git show -s --format=%ct "${version}^{commit}")
	export SOURCE_DATE_EPOCH
fi

man_dir="$work/man"
mkdir -p "$man_dir"
go run ./cmd/gen-man -version "$version" -output "$man_dir"

targets='linux amd64
linux arm64
darwin amd64
darwin arm64
windows amd64'

printf '%s\n' "$targets" | while read -r goos goarch; do
	stage="$work/stage-$goos-$goarch"
	mkdir -p "$stage/bin"
	binary=callbell
	archive="callbell_${version}_${goos}_${goarch}.tar.gz"
	if [ "$goos" = windows ]; then
		binary=callbell.exe
		archive="callbell_${version}_${goos}_${goarch}.zip"
	fi

	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath \
		-ldflags "-s -w -X github.com/castrowithcee/callbell-cli/internal/cli.version=$version" \
		-o "$stage/bin/$binary" .
	cp LICENSE "$stage/LICENSE"
	if [ "$goos" != windows ]; then
		mkdir -p "$stage/share/man/man1"
		cp "$man_dir"/*.1 "$stage/share/man/man1/"
		tar -C "$stage" -czf "$output/$archive" .
	else
		(cd "$stage" && zip -q -r "$output/$archive" .)
	fi
done

if command -v sha256sum >/dev/null 2>&1; then
	(cd "$output" && sha256sum callbell_*) > "$output/checksums.txt"
else
	(cd "$output" && shasum -a 256 callbell_*) > "$output/checksums.txt"
fi
