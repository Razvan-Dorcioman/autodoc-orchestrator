BIN := bin/autodoc

.PHONY: build test vet check smoke clean

build:
	go build -o $(BIN) ./cmd/autodoc

test:
	go test ./...

vet:
	go vet ./...

check: vet test

# Needs a model backend (see .env.example). Asks it to read this repo's go.mod via the read_file tool.
smoke:
	go run ./cmd/autodoc smoke

clean:
	rm -rf bin
