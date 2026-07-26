.PHONY: build clean test install

BIN_DIR := dist
SRC_DIR := .

build:
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o $(BIN_DIR)/model-gatewayd ./$(SRC_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o $(BIN_DIR)/model-gatewayd-arm64 ./$(SRC_DIR)
	@echo "build done: $(BIN_DIR)/model-gatewayd (amd64), $(BIN_DIR)/model-gatewayd-arm64 (arm64)"

clean:
	rm -rf $(BIN_DIR)

test:
	go test ./...

install: build
	install -m 0755 $(BIN_DIR)/model-gatewayd /usr/bin/model-gatewayd
	install -d /etc/config
	install -m 0644 etc/config/model-gateway /etc/config/model-gateway
	install -d /etc/init.d
	install -m 0755 etc/init.d/model-gateway /etc/init.d/model-gateway
	/etc/init.d/model-gateway disable
	@echo "installed, run: /etc/init.d/model-gateway enable && /etc/init.d/model-gateway start"
