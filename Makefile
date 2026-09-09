.PHONY: bootstrap kind-up db-up db-down api release-worker web generate check test test-kind

bootstrap: kind-up db-up
	corepack pnpm install --frozen-lockfile

kind-up:
	./scripts/setup-kind.sh

db-up:
	docker compose up -d --wait postgres redis

db-down:
	docker compose down

api:
	go run ./cmd/orbitops-api

release-worker:
	go run ./cmd/orbitops-release-worker

web:
	corepack pnpm --filter @orbitops/web dev

generate:
	go generate ./api
	corepack pnpm web:generate

check: generate
	go vet ./...
	corepack pnpm web:check
	corepack pnpm web:build

test:
	go test ./... -count=1
	corepack pnpm web:test:e2e

test-kind:
	ORBITOPS_NAMESPACE=orbitops-s3 ./scripts/setup-kind.sh
	ORBITOPS_NAMESPACE=orbitops-s3 ORBITOPS_KIND_E2E=1 go test ./test/kind -count=1 -v
