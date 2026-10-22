#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
exec go run -tags=tools ./scripts/mutation "$@"
