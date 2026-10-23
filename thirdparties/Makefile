# The parser dependency uses a statically linked, namespaced jemalloc.
# Read its pinned source archive from the verified Go module, never build inside
# the module cache or borrow another checkout's native libraries.
GO ?= go
JEMALLOC_SHA256 := 3826bc80232f22ed5c4662f3034f799ca316e819103bdc7bb99018a421706f92
BUILD_DIR := _jemalloc_build

.PHONY: all
all: install/lib/libjemalloc.a

install/lib/libjemalloc.a: Makefile ../go.mod ../go.sum
	$(GO) mod download github.com/matrixorigin/matrixone
	@set -eu; \
	module_dir="$$($(GO) list -mod=readonly -m -f '{{.Dir}}' github.com/matrixorigin/matrixone)"; \
	archive="$$module_dir/thirdparties/jemalloc-5.3.1.tar.bz2"; \
	actual="$$(shasum -a 256 "$$archive" | awk '{print $$1}')"; \
	test "$$actual" = "$(JEMALLOC_SHA256)" || { echo "jemalloc source checksum mismatch: $$actual" >&2; exit 1; }; \
	mkdir -p $(BUILD_DIR) install/include install/lib; \
	tar xjf "$$archive" -C $(BUILD_DIR) --strip-components=1
	cd $(BUILD_DIR) && CC="$(CC)" CFLAGS="-O2 -fPIC" ./configure --disable-shared --enable-static --enable-stats --with-jemalloc-prefix=mo_je_ --with-private-namespace=mo_je_
	$(MAKE) -C $(BUILD_DIR) build_lib
	cp $(BUILD_DIR)/include/jemalloc/jemalloc.h install/include/jemalloc.h
	cp $(BUILD_DIR)/lib/libjemalloc.a $@
