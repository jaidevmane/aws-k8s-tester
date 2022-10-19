#!/usr/bin/env bash
set -e

if ! [[ "$0" =~ test/fmt_test.sh ]]; then
  echo "must be run from repository root"
  exit 255
fi

make clean

echo "Running fmt tests..."
IGNORE_PKGS="(vendor)"
FORMATTABLE=$(find . -name \*.go | while read -r a; do echo "$(dirname "$a")/*.go"; done | sort | uniq | grep -vE "$IGNORE_PKGS" | sed "s|\./||g")
FMT=($FORMATTABLE)

function gofmt_pass {
	fmtRes=$(gofmt -l -s -d "${FMT[@]}")
	if [ -n "${fmtRes}" ]; then
		echo -e "gofmt checking failed:\\n${fmtRes}"
		exit 1
	fi
}

gofmt_pass