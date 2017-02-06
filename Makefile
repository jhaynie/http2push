.PHONY: protoc fmt vet linux alpine osx clean run all

SHELL := /bin/bash
BASEDIR := $(shell echo $${PWD})
EXCLUDE_FILES_FILTER := -not -path './vendor/*' -not -path './.git/*' -not -path './.glide/*'
EXCLUDE_DIRS_FILTER := $(EXCLUDE_FILES_FILTER) -not -path '.' -not -path './vendor/*' -not -path './.git' -not -path './.glide'
PROTO_SRC := $(shell find . -type f -name '*.proto' -not -path './vendor/*' -not -path './.git/*')
SRC := $(shell find . -type f -name '*.go' -not -path './vendor/*' -not -path './.git/*')
TEST_PACKAGES := $(shell find . -type f -name '*_test.go' -not -path './tests/*' $(EXCLUDE_DIRS_FILTER) -exec dirname {} \; | sort -u)
BUILD := $(shell git rev-parse --short HEAD | cut -c1-8)
COMMITSHA := $(shell git rev-parse HEAD)
BUILD_DATE = $(shell date +%FT%T%z)
VERSION := $(shell cat $(BASEDIR)/VERSION)
NAME := http2push
BASEPKG := jhaynie/$(NAME)
PKGMAIN := main.go

LDFLAGS = -ldflags "-X github.com/$(BASEPKG)/main.Build=$(BUILD) -X github.com/$(BASEPKG)/main.BuildDate=$(BUILD_DATE) -X github.com/$(BASEPKG)/main.Version=$(VERSION)"

all: clean fmt vet docker

clean:
	@rm -rf build

linux:
	@docker run --rm -v $(GOPATH):/go -w /go/src/github.com/$(BASEPKG) golang:1.8 go build $(LDFLAGS) -o build/linux/$(NAME)-linux-$(VERSION) $(PKGMAIN)

alpine:
	@docker run --rm -v $(GOPATH):/go -w /go/src/github.com/$(BASEPKG) jhaynie/golang-alpine go build $(LDFLAGS) -o build/alpine/$(NAME)-alpine-$(VERSION) $(PKGMAIN)

osx:
	@docker run -e GOOS=darwin -e GOARCH=amd64 --rm -v $(GOPATH):/go -w /go/src/github.com/$(BASEPKG) golang:1.8 go build $(LDFLAGS) -o build/osx/$(NAME)-osx-$(VERSION) $(PKGMAIN)

docker: clean alpine
	@docker build --squash --build-arg VERSION=$(VERSION) --build-arg COMMITSHA=$(COMMITSHA) -t $(BASEPKG) .

fmt:
	@gofmt -s -l -w $(SRC)

vet:
	@for i in `find . -type d -not -path './vendor/*' -not -path './.git/*' -not -path './.git' -not -path './vendor' -not -path '.' -not -path '*/testdata' -not -path './cmd' -not -path './.*' -not -path './build/*' -not -path './backup' -not -path './vendor' -not -path '.' -not -path './build' -not -path './etc' -not -path './etc/*' -not -path './pkg' | sed 's/^\.\///g'`; do go vet github.com/$(BASEPKG)/$$i; done

run: docker
	@docker run -it --rm \
		-v $(PWD)/testdata/cert.crt:/cert.crt \
		-v $(PWD)/testdata/cert.key:/cert.key \
		-v $(PWD)/testdata:/app/html \
		-p 9998:9998 $(BASEPKG) --selfsigned
