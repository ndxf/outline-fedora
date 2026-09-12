.PHONY: build test tidy clean version release release-tarball release-rpm release-deb release-sign release-checksums install

# ---------------------------------------------------------------------------
# Version stamping.
#
# VERSION file at repo root is authoritative. Bump manually before release.
# Untagged/dirty builds get a +git.<sha>[-dirty] suffix in FULL_VERSION so
# `ouf --version` can distinguish "the released 0.1.0" from "0.1.0 with
# uncommitted local changes".
# ---------------------------------------------------------------------------
VERSION := $(shell cat VERSION)
GIT_REV := $(shell git rev-parse --short=8 HEAD 2>/dev/null || echo unknown)
GIT_DIRTY := $(shell git diff --quiet 2>/dev/null || echo -dirty)
FULL_VERSION := $(VERSION)+git.$(GIT_REV)$(GIT_DIRTY)

BUILD_DIR := build
DIST_DIR := dist
PKG_NAME := outline-fedora
ARCH := amd64
TARBALL_NAME := $(PKG_NAME)-$(VERSION)-linux-$(ARCH).tar.gz
TARBALL := $(DIST_DIR)/$(TARBALL_NAME)
DEB_NAME := $(PKG_NAME)_$(VERSION)_$(ARCH).deb
DEB := $(DIST_DIR)/$(DEB_NAME)

# LDFLAGS: -s -w strip DWARF/symbol tables so the binaries are ~30% smaller
# and don't carry function names; -X injects the version string; -trimpath
# in GOFLAGS makes builds reproducible byte-for-byte across machines.
LDFLAGS := -s -w -X main.version=$(FULL_VERSION)
GOFLAGS := -trimpath -ldflags='$(LDFLAGS)'

# GPG key for signing. Override with `make release GPG_KEY=<fpr>`.
GPG_KEY ?= 162AB0783E8CE396

# ---------------------------------------------------------------------------
# Dev build.
# ---------------------------------------------------------------------------
build:
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/ouf ./cmd/ouf
	go build -o $(BUILD_DIR)/oufdee ./cmd/oufdee

test:
	go test ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR) $(DIST_DIR)

version:
	@echo "VERSION       = $(VERSION)"
	@echo "GIT_REV       = $(GIT_REV)"
	@echo "FULL_VERSION  = $(FULL_VERSION)"

# ---------------------------------------------------------------------------
# Install (system-wide). Requires root or DESTDIR staging.
# Used by scripts/install.sh; also usable directly.
# ---------------------------------------------------------------------------
install: build
	install -Dm755 $(BUILD_DIR)/ouf     $(DESTDIR)/usr/local/bin/ouf
	install -Dm755 $(BUILD_DIR)/oufdee  $(DESTDIR)/usr/local/sbin/oufdee
	install -Dm755 scripts/ouf-panic    $(DESTDIR)/usr/local/sbin/ouf-panic
	install -Dm644 systemd/oufdee.service $(DESTDIR)/etc/systemd/system/oufdee.service
	install -d -m 0755 $(DESTDIR)/etc/outline-fedora
	install -d -m 0700 $(DESTDIR)/var/lib/outline-fedora
	install -d -m 0700 $(DESTDIR)/var/lib/outline-fedora/backup

# ---------------------------------------------------------------------------
# release: builds every artifact, checksums them, signs them all.
# Refuses to run on a dirty tree so releases are always reproducible from
# a specific commit.
# ---------------------------------------------------------------------------
release: release-tarball release-rpm release-deb release-checksums release-sign
	@echo
	@echo "==> release artifacts:"
	@ls -la $(DIST_DIR)/*.tar.gz $(DIST_DIR)/*.rpm $(DIST_DIR)/*.deb $(DIST_DIR)/SHA256SUMS $(DIST_DIR)/*.asc 2>/dev/null || true

release-tarball:
	@if [ -n "$(GIT_DIRTY)" ]; then \
	    echo "FATAL: working tree is dirty. Commit or stash before releasing."; \
	    exit 2; \
	fi
	@mkdir -p $(DIST_DIR)
	@echo "==> building stripped release binaries for $(FULL_VERSION)"
	CGO_ENABLED=1 go build $(GOFLAGS) -o $(BUILD_DIR)/ouf     ./cmd/ouf
	CGO_ENABLED=1 go build $(GOFLAGS) -o $(BUILD_DIR)/oufdee  ./cmd/oufdee
	@echo "==> assembling tarball"
	@rm -rf $(DIST_DIR)/$(PKG_NAME)-$(VERSION)
	@mkdir -p $(DIST_DIR)/$(PKG_NAME)-$(VERSION)
	@cp $(BUILD_DIR)/ouf     $(DIST_DIR)/$(PKG_NAME)-$(VERSION)/
	@cp $(BUILD_DIR)/oufdee  $(DIST_DIR)/$(PKG_NAME)-$(VERSION)/
	@cp scripts/ouf-panic    $(DIST_DIR)/$(PKG_NAME)-$(VERSION)/
	@cp scripts/install.sh   $(DIST_DIR)/$(PKG_NAME)-$(VERSION)/
	@cp scripts/uninstall.sh $(DIST_DIR)/$(PKG_NAME)-$(VERSION)/
	@cp systemd/oufdee.service $(DIST_DIR)/$(PKG_NAME)-$(VERSION)/
	@cp README.md LICENSE VERSION $(DIST_DIR)/$(PKG_NAME)-$(VERSION)/
	@cp -r docs $(DIST_DIR)/$(PKG_NAME)-$(VERSION)/
	@tar -C $(DIST_DIR) --sort=name --owner=0 --group=0 \
	    --mtime='UTC 2020-01-01' --numeric-owner \
	    -czf $(TARBALL) $(PKG_NAME)-$(VERSION)
	@rm -rf $(DIST_DIR)/$(PKG_NAME)-$(VERSION)
	@echo "==> $(TARBALL)"

# ---------------------------------------------------------------------------
# RPM: uses the pre-built tarball as Source0. No compilation happens inside
# rpmbuild — the binaries are already built by release-tarball. This means
# rpmbuild only needs the packaging tooling, not the Go toolchain.
# ---------------------------------------------------------------------------
release-rpm: release-tarball
	@command -v rpmbuild >/dev/null 2>&1 || { \
	    echo "FATAL: rpmbuild not installed. Install with:"; \
	    echo "  sudo dnf install rpm-build rpmdevtools systemd-rpm-macros"; \
	    exit 2; \
	}
	@echo "==> building RPM"
	@mkdir -p $(DIST_DIR)/rpmbuild/{BUILD,BUILDROOT,RPMS,SOURCES,SPECS,SRPMS}
	@cp $(TARBALL) $(DIST_DIR)/rpmbuild/SOURCES/
	@sed 's/@VERSION@/$(VERSION)/g' packaging/outline-fedora.spec.in \
	    > $(DIST_DIR)/rpmbuild/SPECS/outline-fedora.spec
	rpmbuild --define "_topdir $(abspath $(DIST_DIR))/rpmbuild" \
	         -bb $(DIST_DIR)/rpmbuild/SPECS/outline-fedora.spec
	@cp $(DIST_DIR)/rpmbuild/RPMS/*/outline-fedora-*.rpm $(DIST_DIR)/
	@echo "==> $$(ls $(DIST_DIR)/outline-fedora-$(VERSION)-*.rpm)"

# ---------------------------------------------------------------------------
# DEB: hand-built via dpkg-deb. No debhelper, no debian/rules — the tarball
# already has the binaries, we just stage them into the right layout and
# call dpkg-deb --build. Needs dpkg (available on Fedora).
# ---------------------------------------------------------------------------
release-deb: release-tarball
	@command -v dpkg-deb >/dev/null 2>&1 || { \
	    echo "FATAL: dpkg-deb not installed. Install with:"; \
	    echo "  sudo dnf install dpkg dpkg-dev"; \
	    exit 2; \
	}
	@echo "==> building DEB"
	@rm -rf $(DIST_DIR)/deb-stage
	@mkdir -p $(DIST_DIR)/deb-stage/DEBIAN
	@mkdir -p $(DIST_DIR)/deb-stage/usr/bin
	@mkdir -p $(DIST_DIR)/deb-stage/usr/sbin
	@mkdir -p $(DIST_DIR)/deb-stage/lib/systemd/system
	@mkdir -p $(DIST_DIR)/deb-stage/usr/share/doc/$(PKG_NAME)
	@mkdir -p $(DIST_DIR)/deb-stage/etc/outline-fedora
	@mkdir -p $(DIST_DIR)/deb-stage/var/lib/outline-fedora/backup
	@install -m 0755 $(BUILD_DIR)/ouf              $(DIST_DIR)/deb-stage/usr/bin/ouf
	@install -m 0755 $(BUILD_DIR)/oufdee           $(DIST_DIR)/deb-stage/usr/sbin/oufdee
	@install -m 0755 scripts/ouf-panic             $(DIST_DIR)/deb-stage/usr/sbin/ouf-panic
	@install -m 0644 systemd/oufdee.service        $(DIST_DIR)/deb-stage/lib/systemd/system/oufdee.service
	@install -m 0644 README.md                     $(DIST_DIR)/deb-stage/usr/share/doc/$(PKG_NAME)/README.md
	@install -m 0644 LICENSE                       $(DIST_DIR)/deb-stage/usr/share/doc/$(PKG_NAME)/copyright
	@chmod 0755 $(DIST_DIR)/deb-stage/etc/outline-fedora
	@chmod 0700 $(DIST_DIR)/deb-stage/var/lib/outline-fedora
	@chmod 0700 $(DIST_DIR)/deb-stage/var/lib/outline-fedora/backup
	@sed 's/@VERSION@/$(VERSION)/g' packaging/debian/control.in \
	    > $(DIST_DIR)/deb-stage/DEBIAN/control
	@install -m 0755 packaging/debian/postinst  $(DIST_DIR)/deb-stage/DEBIAN/postinst
	@install -m 0755 packaging/debian/prerm     $(DIST_DIR)/deb-stage/DEBIAN/prerm
	@install -m 0755 packaging/debian/postrm    $(DIST_DIR)/deb-stage/DEBIAN/postrm
	@# Compute installed size (dpkg convention, KiB) so lintian doesn't warn.
	@du -sk $(DIST_DIR)/deb-stage 2>/dev/null | awk '{print "Installed-Size: " $$1}' \
	    >> $(DIST_DIR)/deb-stage/DEBIAN/control
	@fakeroot dpkg-deb --root-owner-group --build $(DIST_DIR)/deb-stage $(DEB) 2>/dev/null \
	  || dpkg-deb --root-owner-group --build $(DIST_DIR)/deb-stage $(DEB)
	@rm -rf $(DIST_DIR)/deb-stage
	@echo "==> $(DEB)"

release-checksums:
	@echo "==> SHA256SUMS"
	@cd $(DIST_DIR) && sha256sum -- $$(ls *.tar.gz *.rpm *.deb 2>/dev/null | sort) > SHA256SUMS
	@cat $(DIST_DIR)/SHA256SUMS

# ---------------------------------------------------------------------------
# Sign every artifact + the SHA256SUMS with the maintainer's GPG key.
# Detached armored signatures side-by-side (Debian/Fedora convention).
# ---------------------------------------------------------------------------
release-sign:
	@command -v gpg >/dev/null 2>&1 || { echo "FATAL: gpg not installed"; exit 2; }
	@gpg --list-keys $(GPG_KEY) >/dev/null 2>&1 || { \
	    echo "FATAL: GPG key $(GPG_KEY) not in keyring"; exit 2; \
	}
	@echo "==> signing with $(GPG_KEY)"
	@cd $(DIST_DIR) && for f in *.tar.gz *.rpm *.deb SHA256SUMS; do \
	    [ -f "$$f" ] || continue; \
	    gpg --local-user $(GPG_KEY) --detach-sign --armor --yes --output "$$f.asc" "$$f"; \
	    echo "    signed: $$f.asc"; \
	done
