.PHONY: build run test vet fmt clean

BIN := apitoken
ifeq ($(OS),Windows_NT)
BIN := apitoken.exe
endif

build:
	go build -o $(BIN) ./cmd/gateway

run:
	go run ./cmd/gateway -config config.yaml

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	del /q $(BIN) 2>nul || rm -f $(BIN)
