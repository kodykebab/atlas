.PHONY: build test vet clean

build:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/atlas-docker-adapter-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o dist/atlas-docker-adapter-linux-arm64 .

test:
	go test -race -count=1 -timeout=60s ./...

vet:
	go vet ./...

clean:
	rm -rf dist
