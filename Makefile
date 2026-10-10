.PHONY: web build test docker dev
web:
	cd web && npm ci && npm run build
build: web
	go build -o consolidarr ./cmd/consolidarr
test:
	go vet ./... && go test ./...
docker:
	docker build -t consolidarr:dev .
dev:
	CONSOLIDARR_DATA=./data CONSOLIDARR_AUTH_DISABLED=true go run ./cmd/consolidarr
