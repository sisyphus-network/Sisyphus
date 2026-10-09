PROTO_FILES := $(wildcard proto/sisyphus/v1/*.proto) $(wildcard proto/sisyphus/node/v1/*.proto)
COVERED := ./apps/...,./packages/ai/...,./packages/geo/...,./packages/hardware/...,./packages/identity/...,./packages/ipfscluster/...,./packages/job-model/...,./packages/kubo/...,./packages/nodedb/...,./packages/runtime/...,./packages/s3/...,./packages/sealed/...,./packages/storage/...

.PHONY: build test cover demo proto tools fmt

build:
	go build -o bin/sisyphusd ./apps/sisyphusd

test:
	go vet ./...
	go test -race ./...

# Fails unless the tests execute every statement of hand-written code.
# Generated protocol code is not counted.
cover:
	go test -count=1 -coverpkg=$(COVERED) -coverprofile=coverage.out ./...
	./scripts/check-coverage.sh coverage.out

# Runs a three-node pool and a job on this machine, in one terminal.
demo:
	./scripts/demo.sh

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
