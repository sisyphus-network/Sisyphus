PROTO_FILES := $(wildcard proto/sisyphus/v1/*.proto)

.PHONY: build test proto tools fmt

build:
	go build -o bin/sisyphusd ./apps/sisyphusd

test:
	go vet ./...
	go test -race ./...

fmt:
	gofmt -w apps packages

# Regenerates packages/protocol from proto/. Needs protoc and `make tools`.
proto:
	protoc -I proto \
		--go_out=packages/protocol --go_opt=paths=source_relative \
		--go-grpc_out=packages/protocol --go-grpc_opt=paths=source_relative \
		$(PROTO_FILES)

tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
