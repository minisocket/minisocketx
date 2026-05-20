.PHONY: all server client clean build-all deploy

SERVER   = minisocketx-server
DOMAIN   = pty.minisocket.io
PROD_IP  = 157.230.255.170
PROD_BIN = /usr/local/bin/minisocketx-server
PROD_DL  = /var/www/$(DOMAIN)/bin
LDFLAGS  = -s -w

all: server client

server:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(SERVER) .

client:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o minisocketx-linux-amd64 ./client/

build-all: server
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o minisocketx-linux-amd64 ./client/
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o minisocketx-linux-arm64 ./client/
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o minisocketx-darwin-amd64 ./client/
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o minisocketx-darwin-arm64 ./client/

deploy: build-all
	scp $(SERVER) root@$(PROD_IP):/tmp/$(SERVER)
	ssh root@$(PROD_IP) "mv /tmp/$(SERVER) $(PROD_BIN) && systemctl restart minisocketx.service"
	scp minisocketx-linux-amd64  root@$(PROD_IP):$(PROD_DL)/
	scp minisocketx-linux-arm64  root@$(PROD_IP):$(PROD_DL)/
	scp minisocketx-darwin-amd64 root@$(PROD_IP):$(PROD_DL)/
	scp minisocketx-darwin-arm64 root@$(PROD_IP):$(PROD_DL)/
	@echo "Deployed to $(DOMAIN)"

clean:
	rm -f $(SERVER) minisocketx-linux-* minisocketx-darwin-*

run:
	go run . -addr :3337 -domain localhost:3337
