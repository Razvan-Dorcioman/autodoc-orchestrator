PORT ?= 8080

.PHONY: run test

run:
	go run ./cmd/server

test: vet
	go test ./...
