# AyeusANN Makefile
# Usage: make dev          — start full development environment
#        make lint          — run all linters
#        make test          — run all tests
#        make build         — build all services
#        make migrate       — run database migrations
#        make migrate-down  — rollback last migration
#        make seed          — seed the database
#        make proto         — generate protobuf code
#        make clean         — clean build artifacts

.PHONY: help dev dev-infra dev-services dev-stop build build-go build-agent \
        test test-go test-agent lint lint-go lint-agent lint-proto \
        migrate migrate-down seed proto clean fmt

# ─── Variables ────────────────────────────────────────────────

COMPOSE_FILE := deploy/compose/docker-compose.yml
DB_URL := postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable

GO_SERVICES := gateway control-api scheduler coordinator router inference-gateway billing-meter trust-engine
GO_SVC_DIRS := $(addprefix services/,$(GO_SERVICES))

# ─── Help ─────────────────────────────────────────────────────

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ─── Development Environment ─────────────────────────────────

dev: dev-infra migrate seed dev-services ## Start full development environment
	@echo "\n✅ AyeusANN dev environment is running!"
	@echo "   Postgres:  localhost:5432"
	@echo "   Redis:     localhost:6379"
	@echo "   NATS:      localhost:4222 (monitoring: 8222)"
	@echo "   MinIO:     localhost:9000 (console: 9001)"
	@echo "   Gateway:   localhost:8080"
	@echo "   Control:   localhost:8081"
	@echo "   Scheduler: localhost:8082"
	@echo "   Coord:     localhost:8083"
	@echo "   Router:    localhost:8084"
	@echo "   InfGW:     localhost:8085"
	@echo "   Billing:   localhost:8086"
	@echo "   Trust:     localhost:8087"

dev-infra: ## Start infrastructure (PG, Redis, NATS, MinIO)
	docker compose -f $(COMPOSE_FILE) up -d
	@echo "Waiting for Postgres to be ready..."
	@until docker exec ann-postgres pg_isready -U ayeusann > /dev/null 2>&1; do sleep 1; done
	@echo "Infrastructure ready."

dev-services: ## Start all Go services in background
	@for svc in $(GO_SERVICES); do \
		echo "Starting $$svc..."; \
		SN_ENV=dev go run ./services/$$svc/ & \
	done
	@echo "All services starting..."
	@sleep 2

dev-stop: ## Stop all development processes
	docker compose -f $(COMPOSE_FILE) down
	@pkill -f "go run ./services/" 2>/dev/null || true
	@echo "Dev environment stopped."

# ─── Build ────────────────────────────────────────────────────

build: build-go build-agent ## Build all services

build-go: ## Build all Go services
	@mkdir -p bin
	@for svc in $(GO_SERVICES); do \
		echo "Building $$svc..."; \
		CGO_ENABLED=0 go build -o bin/$$svc ./services/$$svc/; \
	done
	@echo "✅ All Go services built in ./bin/"

build-agent: ## Build the Rust host agent
	cd agent && cargo build --release
	@cp agent/target/release/AyeusANN-agent bin/spazenode-agent 2>/dev/null || true
	@echo "✅ Agent built"

# ─── Test ─────────────────────────────────────────────────────

test: test-go ## Run all tests

test-go: ## Run Go tests
	go test -race -count=1 ./...

test-agent: ## Run Rust agent tests
	cd agent && cargo test

# ─── Lint ─────────────────────────────────────────────────────

lint: lint-go lint-proto ## Run all linters

lint-go: ## Lint Go code
	@if command -v golangci-lint > /dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not found, running go vet..."; \
		go vet ./...; \
	fi

lint-agent: ## Lint Rust agent
	cd agent && cargo clippy -- -D warnings

lint-proto: ## Lint protobuf definitions
	buf lint

# ─── Format ───────────────────────────────────────────────────

fmt: ## Format all code
	gofmt -w .
	cd agent && cargo fmt
	buf format -w

# ─── Database ─────────────────────────────────────────────────

migrate: ## Run database migrations
	migrate -path schema/migrations -database "$(DB_URL)" up

migrate-down: ## Rollback the last migration
	migrate -path schema/migrations -database "$(DB_URL)" down 1

migrate-force: ## Force migration version (use: make migrate-force V=1)
	migrate -path schema/migrations -database "$(DB_URL)" force $(V)

seed: ## Seed the database with initial data
	@for f in schema/seeds/*.sql; do \
		echo "Seeding: $$f"; \
		docker exec -i ann-postgres psql -U ayeusann -d ayeusann < "$$f" 2>/dev/null || true; \
	done

# ─── Protobuf ─────────────────────────────────────────────────

proto: ## Generate protobuf code
	buf generate
	@echo "✅ Protobuf code generated in gen/"

# ─── Clean ────────────────────────────────────────────────────

clean: ## Clean build artifacts
	rm -rf bin/
	rm -rf gen/
	cd agent && cargo clean
	@echo "✅ Cleaned"

# ─── Health Check ─────────────────────────────────────────────

check: ## Check health of all services
	@echo "Checking service health..."
	@for port in 8080 8081 8082 8083 8084 8085 8086 8087; do \
		status=$$(curl -s -o /dev/null -w '%{http_code}' http://localhost:$$port/healthz 2>/dev/null); \
		if [ "$$status" = "200" ]; then \
			echo "  ✅ :$$port — healthy"; \
		else \
			echo "  ❌ :$$port — status $$status"; \
		fi; \
	done
