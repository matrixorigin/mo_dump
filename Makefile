# Copyright 2021 - 2022 Matrix Origin
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

#
# Examples
#
# By default, make builds the mo-service
#
# make
#
# To re-build MO -
#
#	make clean
#	make build
#
# To re-build MO in debug mode also with race detector enabled -
#
# make clean
# make debug
#
# To run static checks -
#
# make install-static-check-tools
# make static-check
#
# To construct a directory named vendor in the main module’s root directory that contains copies of all packages needed to support builds and tests of packages in the main module.
# make vendor
#

# where am I
ROOT_DIR = $(shell dirname $(realpath $(lastword $(MAKEFILE_LIST))))
MO_DUMP := mo-dump
GO := env GOTOOLCHAIN=go$(shell awk '$$1 == "go" {print $$2}' $(ROOT_DIR)/go.mod) GOWORK=off go
UNAME_S := $(shell uname -s)
GOPATH := $(shell $(GO) env GOPATH)
GO_VERSION=$(shell $(GO) version)
BRANCH_NAME=$(shell git rev-parse --abbrev-ref HEAD)
LAST_COMMIT_ID=$(shell git rev-parse --short HEAD)
BUILD_TIME=$(shell date +%s)
MO_VERSION=$(shell git describe --always --tags $(shell git rev-list --tags --max-count=1))
GO_MODULE=$(shell $(GO) list -m)

# cross compilation has been disabled for now
ifneq ($(GOARCH)$(TARGET_ARCH)$(GOOS)$(TARGET_OS),)
$(error cross compilation has been disabled)
endif

###############################################################################
# default target
###############################################################################

all: build

###############################################################################
# build vendor directory
###############################################################################

.PHONY: vendor-build
vendor-build:
	$(info [go mod vendor])
	@$(GO) mod vendor


###############################################################################
# clean
###############################################################################

.PHONY: clean
clean:
	$(info [Clean up])
	$(info Clean go test cache)
	@$(GO) clean -testcache
	rm -f $(MO_DUMP)


###############################################################################
# modump
###############################################################################

RACE_OPT :=
DEBUG_OPT :=
CGO_DEBUG_OPT :=
CGO_OPTS=CGO_CFLAGS="-I$(ROOT_DIR)/thirdparties/install/include" CGO_LDFLAGS="-L$(ROOT_DIR)/thirdparties/install/lib"
GOLDFLAGS=-ldflags="-X '$(GO_MODULE)/pkg/version.GoVersion=$(GO_VERSION)' -X '$(GO_MODULE)/pkg/version.BranchName=$(BRANCH_NAME)' -X '$(GO_MODULE)/pkg/version.CommitID=$(LAST_COMMIT_ID)' -X '$(GO_MODULE)/pkg/version.BuildTime=$(BUILD_TIME)' -X '$(GO_MODULE)/pkg/version.Version=$(MO_VERSION)'"

.PHONY: cgo
cgo:
	@(cd cgo; ${MAKE} ${CGO_DEBUG_OPT})

.PHONY: jemalloc
jemalloc:
	$(MAKE) -C thirdparties GO="$(GO)"

.PHONY: modump
modump: jemalloc
	$(CGO_OPTS) $(GO) build $(RACE_OPT) $(GOLDFLAGS) -o $(MO_DUMP) ./cmd/mo-dump

.PHONY: build
build: modump

.PHONY: test
test: jemalloc
	$(CGO_OPTS) $(GO) test -mod=readonly -count=1 ./...

###############################################################################
# static checks
###############################################################################
