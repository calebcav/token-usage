.PHONY: build test race vet check clean

TOKEN_USAGE_VERSION ?= 0.1.0
TOKEN_USAGE_GO_LDFLAGS ?= -s -w -X main.version=$(TOKEN_USAGE_VERSION)

build:
	go build -trimpath -ldflags "$(TOKEN_USAGE_GO_LDFLAGS)" -o bin/token-usage ./cmd/token-usage

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

check: test vet build

clean:
	rm -f bin/token-usage coverage.out
