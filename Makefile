PREFIX ?= /usr

.PHONY: all build static test vet install clean

all: build

build:
	go build -o sinty-cored ./cmd/sinty-cored
	go build -o sinty-screen ./cmd/sinty-screen

static:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o sinty-cored ./cmd/sinty-cored
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o sinty-screen ./cmd/sinty-screen

test:
	go test ./...

vet:
	go vet ./...

install: static
	install -Dm755 sinty-cored $(DESTDIR)$(PREFIX)/libexec/sinty-cored
	install -Dm755 sinty-screen $(DESTDIR)$(PREFIX)/bin/sinty-screen
	install -Dm644 packaging/sinty-cored.service $(DESTDIR)$(PREFIX)/lib/systemd/system/sinty-cored.service
	install -Dm644 packaging/sinty-screen.service $(DESTDIR)$(PREFIX)/lib/systemd/system/sinty-screen.service
	install -Dm644 packaging/sinty-cored.conf $(DESTDIR)$(PREFIX)/share/dbus-1/system.d/sinty-cored.conf

clean:
	rm -f sinty-cored sinty-screen
