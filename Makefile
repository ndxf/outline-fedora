.PHONY: build test tidy clean

BUILD_DIR := build

build:
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/ouf ./cmd/ouf
	go build -o $(BUILD_DIR)/oufdee ./cmd/oufdee

test:
	go test ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR)
