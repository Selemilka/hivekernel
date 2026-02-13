.PHONY: all build run proto clean test

BINARY=bin/hivekernel
PROTO_DIR=api/proto
GO_PROTO_OUT_AGENT=api/proto/agentpb
GO_PROTO_OUT_CORE=api/proto/corepb

all: proto build

# Generate Go code from proto files
proto:
	@mkdir -p $(GO_PROTO_OUT_AGENT) $(GO_PROTO_OUT_CORE)
	protoc \
		--proto_path=$(PROTO_DIR) \
		--go_out=$(GO_PROTO_OUT_AGENT) --go_opt=paths=source_relative \
		--go-grpc_out=$(GO_PROTO_OUT_AGENT) --go-grpc_opt=paths=source_relative \
		$(PROTO_DIR)/agent.proto
	protoc \
		--proto_path=$(PROTO_DIR) \
		--go_out=$(GO_PROTO_OUT_CORE) --go_opt=paths=source_relative \
		--go-grpc_out=$(GO_PROTO_OUT_CORE) --go-grpc_opt=paths=source_relative \
		$(PROTO_DIR)/core.proto

build:
	go build -o $(BINARY) ./cmd/hivekernel

run: build
	./$(BINARY)

test:
	go test ./...

clean:
	rm -rf bin/
	rm -rf $(GO_PROTO_OUT_AGENT) $(GO_PROTO_OUT_CORE)

# Install protoc plugins (run once)
tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
