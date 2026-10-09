.PHONY: build run test test-web vet fmt clean

BIN := apitoken
ifeq ($(OS),Windows_NT)
BIN := apitoken.exe
endif

build:
	go build -o $(BIN) ./cmd/gateway

run:
	go run ./cmd/gateway -config config.yaml

# 后端测试（无需 Node）
test:
	go test ./...

# 前端交互回归测试（可选，需要 Node）
test-web:
	node tools/webui_test.js

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	del /q $(BIN) 2>nul || rm -f $(BIN)