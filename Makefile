test:
	go test -trimpath -race ./...

lint:
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

fmt:
	goimports -w .
	go fmt ./...

build:
	mkdir -p tmp
	go build -trimpath -o tmp/ ./cmd/burkebot

.PHONY: test lint fmt build
