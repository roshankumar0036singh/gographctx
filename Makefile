.PHONY: build install fmt fmt-check vet lint test short race bench bench-suite cover clean

BINARY := gographctx
ifeq ($(OS),Windows_NT)
	BINARY := gographctx.exe
endif

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/gographctx

install:
	CGO_ENABLED=0 go install -trimpath -ldflags="-s -w" ./cmd/gographctx

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "Unformatted files:" && gofmt -l . && exit 1)

vet:
	go vet ./...

lint: fmt-check vet
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "staticcheck not installed; skipping"

short:
	go test -short -count=1 ./...

test:
	go test -count=1 ./...

race:
	CGO_ENABLED=1 go test -race -count=1 ./...

bench:
	go test -run=^$$ -bench=. -benchmem ./internal/graph/... ./internal/bench/...

bench-suite:
	go run ./cmd/ggctx-bench --corpus flask-synth --tools all --assert-sla

cover:
	go test -short -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

clean:
	rm -f gographctx gographctx.exe ggctx-bench ggctx-bench.exe coverage.out *.prof
