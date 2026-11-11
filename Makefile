PREFIX ?= /usr
CC ?= cc

.PHONY: all build static test vet install clean

all: build

build: sinty-v4l2-proxy
	go build -o sinty-cored ./cmd/sinty-cored
	go build -o sinty-screen ./cmd/sinty-screen

sinty-v4l2-proxy: proxy/sinty-v4l2-proxy.c
	$(CC) $(CFLAGS) -O2 -Wall -o $@ $< $(LDFLAGS) -ldl

static: sinty-v4l2-proxy
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o sinty-cored ./cmd/sinty-cored
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o sinty-screen ./cmd/sinty-screen

test:
	go test ./...

vet:
	go vet ./...

install: static
	install -Dm755 sinty-cored $(DESTDIR)$(PREFIX)/libexec/sinty-cored
	install -Dm755 sinty-screen $(DESTDIR)$(PREFIX)/bin/sinty-screen
	install -Dm755 sinty-v4l2-proxy $(DESTDIR)$(PREFIX)/libexec/sinty-v4l2-proxy
	install -Dm644 packaging/sinty-cored.service $(DESTDIR)$(PREFIX)/lib/systemd/system/sinty-cored.service
	install -Dm644 packaging/sinty-screen.service $(DESTDIR)$(PREFIX)/lib/systemd/system/sinty-screen.service
	install -Dm644 packaging/sinty-cored.conf $(DESTDIR)$(PREFIX)/share/dbus-1/system.d/sinty-cored.conf

clean:
	rm -f sinty-cored sinty-screen sinty-v4l2-proxy
