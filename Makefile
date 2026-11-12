.PHONY: all build test lint clean check bench

all: check build

build:
	@echo "==> Building zyg CLI..."
	@go build -o bin/zyg cmd/zyg/main.go

test:
	@echo "==> Running tests..."
	@go test -v -race -cover ./...

lint:
	@echo "==> Running golangci-lint..."
	@golangci-lint run

bench:
	@echo "==> Running bundle import benchmark (NFR-1.7)..."
	@go test -run '^$$' -bench BenchmarkImportBundle100k -benchtime=3x ./trace

clean:
	@echo "==> Cleaning..."
	@rm -rf bin/
	@go clean

check: lint test
	@echo "==> Running TTFV walkthrough..."
	@bash scripts/ttfv_test.sh
	@echo "==> All checks passed!"
