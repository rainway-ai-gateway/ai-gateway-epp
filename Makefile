# Copyright(c) 2024 The Rainway AI Gateway (壬远AI网关) Authors. All rights reserved.
# Copyright (c) 2019 The BFE Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# init epp version
DISTDIR   := $(shell pwd)/dist
HOMEDIR   := $(shell pwd)
OUTDIR    := $(HOMEDIR)/output
EPP_VERSION ?= $(shell cat VERSION | sed 's/^v//')
GIT_COMMIT  ?= $(shell git rev-parse --short HEAD)

GOBUILD ?= go build
GOTEST  ?= go test


# -----------------------------------------------------------------------------
# Container image build variables (used by docker/docker-push targets)
# -----------------------------------------------------------------------------

# Image name and version
IMAGE_NAME ?= ai-gateway-epp
VERSION ?= $(shell cat VERSION | sed 's/^v//')
VERSION_TAG := v$(VERSION)

# Registry and build settings
REGISTRY ?=
PLATFORMS ?= linux/amd64,linux/arm64
BUILDER_NAME ?= ai-gateway-epp-builder
NO_CACHE ?= false
DOCKER_BUILD_ARGS ?=

# Derived tags
IMAGE_LOCAL := $(IMAGE_NAME):$(VERSION_TAG)
IMAGE_LATEST_LOCAL := $(IMAGE_NAME):latest
IMAGE_REMOTE := $(if $(REGISTRY),$(REGISTRY)/$(IMAGE_NAME):$(VERSION_TAG),)
IMAGE_LATEST_REMOTE := $(if $(REGISTRY),$(REGISTRY)/$(IMAGE_NAME):latest,)
# -----------------------------------------------------------------------------


.PHONY: all build test prepare release clean docker docker-push

all: build

# 构建本地二进制（当前平台）
build:
	$(GOBUILD) -ldflags "-X main.version=v$(EPP_VERSION) -X main.commit=$(GIT_COMMIT)" \
		-o ./bin/epp ./cmd/epp

test:
	$(GOTEST) ./...

prepare:
	mkdir -p $(DISTDIR)

# 交叉编译 + 打包 dist tarball，供 GitHub Release 上传
release: prepare
	@for arch in amd64 arm64; do \
		STAGE=$(DISTDIR)/tmp-epp_$(EPP_VERSION)_linux_$$arch; \
		rm -rf $$STAGE; \
		mkdir -p $$STAGE; \
		echo "  -> Building linux/$$arch..."; \
		GOOS=linux GOARCH=$$arch CGO_ENABLED=0 \
			$(GOBUILD) -a -installsuffix cgo \
			-ldflags "-X main.version=v$(EPP_VERSION) -X main.commit=$(GIT_COMMIT)" \
			-o $$STAGE/epp ./cmd/epp; \
		[ -f LICENSE ]   && cp LICENSE   $$STAGE/ || true; \
		[ -f README.md ] && cp README.md $$STAGE/ || true; \
		(cd $$STAGE && tar czvf ../epp_$(EPP_VERSION)_linux_$$arch.tar.gz .); \
		rm -rf $$STAGE; \
		echo "  -> dist/epp_$(EPP_VERSION)_linux_$$arch.tar.gz done"; \
	done
	@echo "Release packages built successfully."

clean:
	rm -rf $(DISTDIR) epp

# make docker
docker:
	docker build $(if $(filter 1 true TRUE True,$(NO_CACHE)),--no-cache,) $(DOCKER_BUILD_ARGS) \
		-t $(IMAGE_LOCAL) \
		-t $(IMAGE_LATEST_LOCAL) \
		.

# make docker-push (multi-arch)
docker-push:
	@test -n "$(REGISTRY)" || (echo "REGISTRY is required, e.g. REGISTRY=ghcr.io/your-org" && exit 1)
	@docker buildx inspect $(BUILDER_NAME) >/dev/null 2>&1 || docker buildx create --name $(BUILDER_NAME) --driver docker-container --use
	@docker buildx use $(BUILDER_NAME)
	@docker buildx inspect --bootstrap >/dev/null
	docker buildx build $(if $(filter 1 true TRUE True,$(NO_CACHE)),--no-cache,) $(DOCKER_BUILD_ARGS) \
		--platform $(PLATFORMS) \
		--push \
		-t $(IMAGE_REMOTE) \
		-t $(IMAGE_LATEST_REMOTE) \
		.
