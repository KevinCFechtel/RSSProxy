.PHONY: build test vet check

build:
	go build -trimpath -o bin/rssproxy ./cmd/rssproxy

test:
	go test -race ./...

vet:
	go vet ./...

check: vet test
	govulncheck ./...
