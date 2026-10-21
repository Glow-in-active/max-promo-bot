#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
target=internal/transport/httpapi/generated/api.gen.go
snapshot=$(mktemp)
trap 'rm -f "$snapshot"' EXIT INT TERM
go tool oapi-codegen -config api/oapi-codegen.yaml -o "$snapshot" api/openapi.yaml
if ! cmp -s "$snapshot" "$target"; then
    printf '%s\n' 'Generated HTTP contract is stale. Run make generate and commit the result.' >&2
    diff -u "$target" "$snapshot" || true
    exit 1
fi
printf '%s\n' 'Generated HTTP contract is up to date.'
