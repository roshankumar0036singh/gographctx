.PHONY: fmt vet lint test race bench cover

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "Unformatted files:" && gofmt -l . && exit 1)

vet:
	go vet ./...

lint: fmt-check vet
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "staticcheck not installed; skipping"

test:
	go test -count=1 ./...

race:
	CGO_ENABLED=1 go test -race -count=1 ./...

bench:
	go test -run=^$$ -bench=. -benchmem ./internal/graph/...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

