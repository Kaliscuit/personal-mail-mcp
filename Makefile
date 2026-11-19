CA_FILE ?= $(firstword $(wildcard /etc/ssl/certs/ca-certificates.crt /etc/ssl/cert.pem /etc/pki/tls/certs/ca-bundle.crt))

.PHONY: test binaries image

test:
	go test -race ./...
	go vet ./...

binaries:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/personal-mail-mcp-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o dist/personal-mail-mcp-linux-arm64 .

image: binaries
	@test -s "$(CA_FILE)" || { echo "Set CA_FILE to a trusted system CA bundle (PEM)." >&2; exit 1; }
	cp "$(CA_FILE)" dist/ca-certificates.crt
	docker buildx build --platform linux/amd64,linux/arm64 -t personal-mail-mcp:local --output type=oci,dest=dist/personal-mail-mcp.oci.tar .
