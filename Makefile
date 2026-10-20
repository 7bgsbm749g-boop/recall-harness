.PHONY: build test check

build:
	go build -trimpath -o bin/recall ./cmd/recall
	go build -trimpath -o bin/unreal-agent-runner ./cmd/unreal-agent-runner
	go -C cmd/unreal-agent-tui build -trimpath -o ../../bin/unreal-agent-tui .

test:
	go test -race ./...
	go -C cmd/unreal-agent-tui test -race ./...

check:
	go vet ./...
	go -C cmd/unreal-agent-tui vet ./...
	@test -z "$$(gofmt -l cmd harness internal recall)" || { gofmt -l cmd harness internal recall; exit 1; }
