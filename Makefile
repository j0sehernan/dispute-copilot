SHELL := /bin/bash
COMPOSE := docker compose
TEST_DATABASE_URL ?= postgres://copilot:copilot@localhost:5433/copilot?sslmode=disable
GOLANGCI_VERSION := v2.5.0

.PHONY: up down logs seed eval eval-report test test-integration lint build clean

## up: build and start the stack (asks for the OpenAI key if it is not set)
up:
	@if [ -z "$$OPENAI_API_KEY" ] && [ -z "$$LLM_API_KEY" ] && [ "$$LLM_PROVIDER" != "ollama" ]; then \
		read -r -s -p "OpenAI API key: " key; echo; export OPENAI_API_KEY="$$key"; \
	fi; \
	$(COMPOSE) up --build -d
	@echo "Open http://localhost:8080"

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f api worker

## seed: load the 15 synthetic disputes (also runs on `up`)
seed:
	$(COMPOSE) run --rm seed

## eval: run every scenario through the running stack and print accuracy, cost and latency
eval:
	$(COMPOSE) --profile eval run --rm eval

## eval-report: same as eval, and write the results table into README.md
eval-report:
	$(COMPOSE) --profile eval run --rm -v $(CURDIR):/repo --user "$$(id -u):$$(id -g)" eval /app/eval -readme /repo/README.md

## test: unit tests (integration tests skip without a database)
test:
	go test -race ./...

## test-integration: unit + integration tests against the compose Postgres
test-integration:
	$(COMPOSE) up -d --wait postgres
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race -count=1 ./...

lint:
	@if command -v golangci-lint >/dev/null; then golangci-lint run; \
	else docker run --rm -v $(CURDIR):/app -w /app golangci/golangci-lint:$(GOLANGCI_VERSION) golangci-lint run; fi

build:
	CGO_ENABLED=0 go build -o bin/ ./cmd/...

clean:
	$(COMPOSE) down -v
	rm -rf bin
