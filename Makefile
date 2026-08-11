.PHONY: build test race lint vet vuln

build:
	go build ./cmd/notes-sync

test:
	go test ./...

race:
	go test -race ./...

lint:
	golangci-lint run

vet:
	go vet ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
