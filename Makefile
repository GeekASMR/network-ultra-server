.PHONY: build test clean lint vet tidy run

VERSION ?= dev
BIN     := bin/network-ultra-server

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.buildVersion=$(VERSION)" -o $(BIN) ./cmd/server

run: build
	./$(BIN) -config ./config.local.toml

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	go vet ./...
	gofmt -l . | tee /dev/stderr | wc -l | grep -q '^0$$'

tidy:
	go mod tidy

clean:
	rm -rf bin dist

cross:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/network-ultra-server-linux-amd64 ./cmd/server
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/network-ultra-server-linux-arm64 ./cmd/server
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/network-ultra-server-windows-amd64.exe ./cmd/server
	cd dist && for f in network-ultra-server-*; do sha256sum "$$f" > "$$f.sha256"; done
