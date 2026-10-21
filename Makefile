PROTO_FILES := $(wildcard proto/sisyphus/v1/*.proto) $(wildcard proto/sisyphus/node/v1/*.proto)
# STATICCHECK is the version of staticcheck that make lint and CI run.
STATICCHECK := 2026.2.2
COVERED := ./apps/...,./packages/ai/...,./packages/geo/...,./packages/hardware/...,./packages/identity/...,./packages/ipfscluster/...,./packages/job-model/...,./packages/jobrecord/...,./packages/kubo/...,./packages/names/...,./packages/natpmp/...,./packages/nodedb/...,./packages/runtime/...,./packages/s3/...,./packages/sealed/...,./packages/storage/...,./packages/stun/...

.PHONY: build test cover demo proto tools fmt lint

build:
	go build -o bin/sisyphusd ./apps/sisyphusd

test:
	go vet ./...
	go test -race -timeout 25m ./...

# Fails unless the tests execute every statement of hand-written code.
# Generated protocol code is not counted. The tests that need Kubo, IPFS
# Cluster or Docker are required, as in CI, so that one missing program
# fails with its name rather than as a list of blocks no test ran. Set any
# of these to nothing to skip those tests, knowing coverage will then fail.
cover: export SISYPHUS_REQUIRE_KUBO ?= 1
cover: export SISYPHUS_REQUIRE_CLUSTER ?= 1
cover: export SISYPHUS_REQUIRE_DOCKER ?= 1
cover:
	go test -tags=realimages -count=1 -timeout 25m -coverpkg=$(COVERED) -coverprofile=coverage.out ./...
	./scripts/check-coverage.sh coverage.out

# Runs a three-node pool and a job on this machine, in one terminal.
demo:
	./scripts/demo.sh

# lint looks for mistakes the compiler and vet let through.
lint:
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK) ./...

fmt:
	gofmt -w apps packages

# Regenerates packages/protocol from proto/, and the desktop's TypeScript
# client in packages/client from the node's local API. Needs protoc, `make
# tools`, and `npm ci` in apps/sisyphus.
proto:
	protoc -I proto \
		--go_out=packages/protocol --go_opt=paths=source_relative \
		--go-grpc_out=packages/protocol --go-grpc_opt=paths=source_relative \
		$(PROTO_FILES)
	npm --prefix apps/sisyphus run proto:generate

tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
