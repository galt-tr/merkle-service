.PHONY: build test lint docker-up docker-down run debug-dashboard scale-test mega-scale-test generate-mega-fixtures

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

mega-scale-test:
	go test -tags scale -v -count=1 -timeout 15m -run TestScaleMega ./test/scale/

generate-mega-fixtures:
	cd test/scale && go run ./cmd/generate-fixtures/ --instances 100 --txids-per-instance 10000 --subtrees 250 --txids-per-subtree 4000 --out testdata-mega --seed 42
