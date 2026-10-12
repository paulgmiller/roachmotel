.PHONY: test build image
test:
	go test ./...
build:
	go build -o build/roachmotel ./cmd/roachmotel
image:
	docker build -t roachmotel:dev .
