#!/bin/sh
set -eu
cd "$(dirname "$0")/.."

if [ -n "${POSTMAN_CLI:-}" ] && [ ! -x "$POSTMAN_CLI" ]; then
    printf '%s\n' 'POSTMAN_CLI must point to an executable Postman CLI binary.' >&2
    exit 1
fi

project="promo-smoke-$$"
export COMPOSE_PROFILES=
export HTTP_PORT=0 MODE=demo LOG_LEVEL=info
export ADMIN_TOKEN=local-administrator-token
export DEMO_TOKEN=local-demo-token
export DATABASE_URL=
export POSTGRES_HOST=postgres POSTGRES_PORT=5432 POSTGRES_DB=promo POSTGRES_USER=promo
export POSTGRES_PASSWORD=localdemo-db-password
export POSTGRES_SSLMODE=disable
export MAX_TOKEN= CAMPAIGN_ID= MAX_BANNER_URL= MAX_CA_FILE=
export MAX_API_URL=https://platform-api2.max.ru
export MAX_WEBHOOK_SECRET=local-webhook-secret
export NO_PROXY=127.0.0.1,localhost,::1 no_proxy=127.0.0.1,localhost,::1

compose() {
    docker compose --env-file .env.example -f compose.yaml -p "$project" "$@"
}
cleanup() {
    status=$?
    if [ "$status" -ne 0 ]; then
        compose logs --no-color >&2 || true
    fi
    compose down --volumes --remove-orphans >/dev/null 2>&1 || true
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

compose up --build --wait app
container=$(compose ps -q app)
test "$(docker inspect --format '{{.Config.User}}' "$container")" = '65532:65532'
test "$(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "$container")" = 'true'
address=$(compose port app 8080)
base_url="http://$address"
curl --fail --silent --show-error --noproxy '*' --max-time 5 "$base_url/healthz" >/dev/null
curl --fail --silent --show-error --noproxy '*' --max-time 5 "$base_url/readyz" >/dev/null

docker exec "$container" /promo healthcheck
BASE_URL="$base_url" python3 -I postman/verify.py
if [ -n "${POSTMAN_CLI:-}" ]; then
    env -u POSTMAN_API_KEY "$POSTMAN_CLI" collection run postman/promo.postman_collection.json \
        --environment postman/local.postman_environment.json \
        --env-var "baseUrl=$base_url" \
        --env-var "adminToken=$ADMIN_TOKEN" \
        --env-var "demoToken=$DEMO_TOKEN" \
        --env-var "webhookSecret=$MAX_WEBHOOK_SECRET" \
        --no-report-events --disable-unicode --ignore-redirects \
        --timeout 120000 --timeout-request 10000 --timeout-script 30000
fi
compose stop --timeout 15 app
test "$(docker inspect --format '{{.State.ExitCode}}' "$container")" = 0
printf '%s\n' 'Runtime smoke passed: migrations, non-root, read-only filesystem, health checks, API scenarios and SIGTERM.'
