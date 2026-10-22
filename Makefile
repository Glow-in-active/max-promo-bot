SHELL := /bin/sh
.DEFAULT_GOAL := help
TEST_RUNNER := go run -tags=tools ./scripts/testsuite

.PHONY: help up down logs test race coverage fmt vet check integration integration-local mutate mutate-integration e2e e2e-local test-tools verify-local smoke build docs docs-check generate check-generated simulator simulator-down simulator-check

help:
	@printf '%s\n' \
	  'make up                Build and start the local demo' \
	  'make down              Stop containers, preserving demo data' \
	  'make check             Check formatting, vet and run unit tests' \
	  'make generate          Regenerate the typed API contract from OpenAPI' \
	  'make check-generated   Verify generated API files match OpenAPI' \
	  'make race              Run tests with the Go race detector' \
	  'make coverage          Write unit coverage to .coverage/unit.out' \
	  'make integration       Run isolated PostgreSQL integration tests in Docker' \
	  'make integration-local Run integration tests with TEST_DATABASE_URL' \
	  'make e2e               Run the HTTP bot lifecycle in Docker' \
	  'make e2e-local         Run the HTTP lifecycle with TEST_DATABASE_URL' \
	  'make mutate            Run domain mutation tests' \
	  'make mutate-integration Verify cross-layer regressions with TEST_DATABASE_URL' \
	  'make verify-local      Run static checks, all integration tests and both mutation suites' \
	  'make smoke             Verify the runtime container and HTTP workflow' \
	  'make simulator         Start the MAX simulator and browser chat at 127.0.0.1:4010' \
	  'make simulator-down    Stop the simulator, preserving its database' \
	  'make simulator-check   Verify the simulator and complete bot workflow' \
	  'make docs              Start local documentation at 127.0.0.1:8000' \
	  'make docs-check        Build documentation with strict validation'

up:
	docker compose up --build --wait app

down:
	docker compose --profile test down --remove-orphans

logs:
	docker compose logs --follow app

build:
	go build -trimpath -o bin/promo ./cmd/promo

test:
	$(TEST_RUNNER) -count=1 ./...

race:
	$(TEST_RUNNER) -race -count=1 ./...

coverage:
	mkdir -p .coverage
	$(TEST_RUNNER) -race -count=1 -covermode=atomic -coverprofile=.coverage/unit.out ./...
	go tool cover -func=.coverage/unit.out

fmt:
	@test -z "$$(gofmt -l $$(find cmd internal scripts -name '*.go' -type f))" || \
	  { gofmt -l $$(find cmd internal scripts -name '*.go' -type f); exit 1; }

vet:
	go vet ./...

check: fmt vet test

integration:
	docker compose --profile test run --build --rm integration

integration-local:
	@test -n "$$TEST_DATABASE_URL" || { printf '%s\n' 'TEST_DATABASE_URL must point to a disposable test database'; exit 1; }
	$(TEST_RUNNER) -tags=integration -race -count=1 -timeout=120s ./...

mutate:
	./scripts/mutate.sh

mutate-integration:
	./scripts/mutate.sh --integration

test-tools:
	$(TEST_RUNNER) -tags=tools -race -count=1 ./scripts/...

e2e:
	docker compose --profile test run --build --rm integration $(TEST_RUNNER) -tags=integration -race -count=1 -timeout=120s -run '^TestMAXEndToEnd' ./internal/app

e2e-local:
	@test -n "$$TEST_DATABASE_URL" || { printf '%s\n' 'TEST_DATABASE_URL must point to a disposable test database'; exit 1; }
	$(TEST_RUNNER) -tags=integration -race -count=1 -timeout=120s -run '^TestMAXEndToEnd' ./internal/app

verify-local: fmt vet check-generated test-tools integration-local mutate mutate-integration

smoke:
	./scripts/smoke.sh

simulator:
	docker compose --env-file .env.example -f dev/max-simulator/compose.yaml up --build --wait

simulator-down:
	docker compose --env-file .env.example -f dev/max-simulator/compose.yaml down --remove-orphans

simulator-check: simulator
	docker build --target test -f dev/max-simulator/Dockerfile -t promo-max-simulator:test .
	docker run --rm --read-only --cap-drop=ALL --security-opt=no-new-privileges --user=65532:65532 promo-max-simulator:test
	python3 -I scripts/simulator-smoke.py


docs:
	docker compose --profile docs up --wait docs

docs-check:
	docker compose --profile docs run --rm --no-deps docs build --strict


generate:
	go generate ./internal/transport/httpapi/generated

check-generated:
	./scripts/check-generated.sh
