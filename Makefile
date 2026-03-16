.PHONY: build test lint docker-up docker-down run debug-dashboard scale-test

build:
	go build ./...

test:
	go test ./...

lint:
	golangci-lint run

docker-up:
	podman-compose up -d

docker-down:
	podman-compose down

run:
	go run ./cmd/merkle-service

debug-dashboard:
	go run ./tools/debug-dashboard

scale-test:
	go test -tags scale -v -count=1 -timeout 10m ./test/scale/
