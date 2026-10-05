# AyeusANN — common tasks. `make help` lists them.

.PHONY: help dev db services web down status build build-go build-agent build-web dist-agent \
        dist-agent-linux dist-agent-windows images prod-up prod-down prod-logs prod-status \
        test test-go test-agent test-web smoke-fake lint lint-go lint-agent lint-proto proto fmt migrate migrate-down clean

GO_SERVICES := gateway control-api scheduler coordinator inference-gateway trust-engine
TEST_DB_URL := postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann_test?sslmode=disable
DEV_DB_URL  := postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann_dev?sslmode=disable

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# ─── Development ─────────────────────────────────────────────

dev: ## Start everything: Postgres, Redis, services, web console
	scripts/dev.sh up

db: ## Create, migrate and seed the dev and test databases
	scripts/dev.sh db

services: ## Rebuild and restart the Go services
	scripts/dev.sh services

web: ## Start the web console dev server on :3000
	scripts/dev.sh web

down: ## Stop services and the console
	scripts/dev.sh down

status: ## Health of every service
	scripts/dev.sh status

migrate: ## Apply migrations to the dev database
	migrate -path schema/migrations -database "$(DEV_DB_URL)" up

migrate-down: ## Roll back the last dev migration
	migrate -path schema/migrations -database "$(DEV_DB_URL)" down 1

# ─── Build ───────────────────────────────────────────────────

build: build-go build-agent build-web ## Build everything

build-go: ## Build Go services into ./bin
	@mkdir -p bin
	@for svc in $(GO_SERVICES); do echo "building $$svc"; CGO_ENABLED=0 go build -o bin/$$svc ./services/$$svc/; done

build-agent: ## Build the host agent (release)
	cd agent && cargo build --release

build-web: ## Production build of the web console
	cd web/console && npm ci --no-audit --no-fund && npm run build

dist-agent: build-agent ## Publish this machine's agent build for /downloads
	@mkdir -p dist/agent
	@os=$$(uname -s | tr A-Z a-z); arch=$$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/'); \
	 cp agent/target/release/ayeusann-agent dist/agent/ayeusann-agent-$$os-$$arch && \
	 echo "published dist/agent/ayeusann-agent-$$os-$$arch"

dist-agent-linux: ## Build the Linux agent in Docker for /downloads (ARCH=amd64|arm64)
	docker build -f agent/Dockerfile --platform linux/$(or $(ARCH),amd64) --output type=local,dest=dist/agent .

dist-agent-windows: ## Cross-compile the Windows agent in Docker for /downloads
	@mkdir -p dist/agent
	docker build -f agent/Dockerfile.windows --output type=local,dest=dist/agent .

# ─── Production (single machine, Docker Compose) ─────────────

PROD_COMPOSE := docker compose -f deploy/compose/docker-compose.prod.yml --env-file deploy/compose/.env.prod

images: ## Build the production images
	$(PROD_COMPOSE) build

prod-up: ## Build, migrate and start the production stack
	@mkdir -p dist/agent
	$(PROD_COMPOSE) up -d --build --wait

prod-down: ## Stop the production stack (data volumes are kept)
	$(PROD_COMPOSE) down

prod-logs: ## Follow production logs
	$(PROD_COMPOSE) logs -f --tail=100

prod-status: ## State and health of every production container
	$(PROD_COMPOSE) ps

# ─── Test and lint ───────────────────────────────────────────

test: test-go test-agent test-web ## Run all tests

test-go: ## Go tests (integration tests use ayeusann_test; run `make db` first)
	TEST_DATABASE_URL="$(TEST_DB_URL)" go test -race -count=1 ./...

test-agent: ## Rust agent tests
	cd agent && cargo test

test-web: ## Type-check the web console
	cd web/console && npx tsc --noEmit

smoke-fake: ## End-to-end tests with no GPU and no Ollama (needs the dev stack running)
	scripts/smoke-fake.sh

lint: lint-go lint-agent lint-proto ## Run all linters

lint-go:
	go vet ./...

lint-agent:
	cd agent && cargo clippy --all-targets -- -D warnings && cargo fmt -- --check

lint-proto:
	buf lint

proto: ## Regenerate protobuf code (needs protoc-gen-go and protoc-gen-go-grpc)
	PATH="$$(go env GOPATH)/bin:$$PATH" buf generate

fmt: ## Format all code
	gofmt -w internal services
	cd agent && cargo fmt

clean: ## Remove build artefacts
	rm -rf bin/* dist web/console/.next
	cd agent && cargo clean
