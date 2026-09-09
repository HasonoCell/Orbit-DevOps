.PHONY: bootstrap kind-up db-up db-down api release-worker build-worker web generate check test test-kind

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

build-worker:
	ORBITOPS_BUILD_REGISTRY_INSECURE=true \
	ORBITOPS_BUILD_DOCKERHUB_MIRROR=orbitops-s4-registry.orbitops-s4-build.svc.cluster.local:5000 \
	ORBITOPS_BUILD_DOCKERHUB_MIRROR_INSECURE=true go run ./cmd/orbitops-build-worker

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
	ORBITOPS_NAMESPACE=orbitops-s3 \
		ORBITOPS_KIND_E2E=1 \
		ORBITOPS_KIND_BUILD_E2E=1 \
		ORBITOPS_KIND_BUILD_PLATFORM="linux/$$(docker exec orbitops-s1-control-plane uname -m | sed -e 's/aarch64/arm64/' -e 's/x86_64/amd64/')" \
		ORBITOPS_KIND_BUILD_REGISTRY=orbitops-s4-registry.orbitops-s4-build.svc.cluster.local:5000 \
		go test ./test/kind -count=1 -v
