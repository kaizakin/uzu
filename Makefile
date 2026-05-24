GO ?= go
BUILD_DIR ?= build
BIN ?= $(BUILD_DIR)/sqlite-api
ROOTFS_DIR ?= rootfs
DB_PATH ?= /data/app.db
GOCACHE ?= $(CURDIR)/.gocache

.PHONY: all static verify-static run clean rootfs image

all: static

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR) $(GOCACHE)

static: $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -tags netgo,osusergo -o $(BIN) ./cmd

run:
	DB_PATH=./app.db $(GO) run ./cmd -listen=:8080

image: static
	bunny build -f bunnyfile -t sqlite-api-unikernel
