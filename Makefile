.PHONY: bootstrap kind-up db-up db-down api release-worker build-worker pipeline-worker web generate check test test-kind

bootstrap: kind-up db-up
	corepack pnpm install --frozen-lockfile

kind-up:
	./scripts/setup-kind.sh

db-up:
	docker compose up -d --wait postgres redis

db-down:
	docker compose down

api:
	go run ./cmd/orbit-devops-api

release-worker:
	go run ./cmd/orbit-devops-release-worker

build-worker:
	ORBIT_DEVOPS_BUILD_REGISTRY_INSECURE=true \
	ORBIT_DEVOPS_BUILD_DOCKERHUB_MIRROR=orbit-devops-s4-registry.orbit-devops-s4-build.svc.cluster.local:5000 \
	ORBIT_DEVOPS_BUILD_DOCKERHUB_MIRROR_INSECURE=true go run ./cmd/orbit-devops-build-worker

pipeline-worker:
	go run ./cmd/orbit-devops-pipeline-worker

web:
	corepack pnpm --filter @orbit-devops/web dev

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
	ORBIT_DEVOPS_NAMESPACE=orbit-devops-s3 ./scripts/setup-kind.sh
	ORBIT_DEVOPS_NAMESPACE=orbit-devops-s3 \
		ORBIT_DEVOPS_KIND_E2E=1 \
		ORBIT_DEVOPS_KIND_BUILD_E2E=1 \
		ORBIT_DEVOPS_KIND_BUILD_PLATFORM="linux/$$(docker exec orbit-devops-s1-control-plane uname -m | sed -e 's/aarch64/arm64/' -e 's/x86_64/amd64/')" \
		ORBIT_DEVOPS_KIND_BUILD_REGISTRY=orbit-devops-s4-registry.orbit-devops-s4-build.svc.cluster.local:5000 \
		go test ./test/kind -count=1 -v
