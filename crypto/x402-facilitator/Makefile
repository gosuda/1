ROOT_DIR := $(dir $(realpath $(lastword $(MAKEFILE_LIST))))

build:
	cd $(ROOT_DIR)cmd && go build -o $(ROOT_DIR)bin/x402-facilitator ./facilitator
	cd $(ROOT_DIR)cmd && go build -o $(ROOT_DIR)bin/x402-client ./client

build-docker:
	docker buildx build \
	--platform linux/amd64,linux/arm64 \
	-t dreamcacao/x402-facilitator:0.0.0 \
	-t dreamcacao/x402-facilitator:latest \
	--push .

generate-api:
	swag init -g api/server.go -o api/swagger --parseDependency

generate-abi:
	abigen --abi $(ROOT_DIR)/scheme/evm/eip3009/eip3009.abi \
		--pkg eip3009 \
		--out $(ROOT_DIR)/scheme/evm/eip3009/eip3009.go
	abigen --abi $(ROOT_DIR)/scheme/evm/permit2/permit2.abi \
		--pkg permit2 \
		--type Permit2 \
		--out $(ROOT_DIR)/scheme/evm/permit2/permit2.go