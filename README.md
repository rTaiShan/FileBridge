# FileBridge

FileBridge tunnels HTTP between a local client and server-local services using an
atomic, file-based message transport on a shared folder.

## Configuration

The server accepts JSON, or the following small YAML configuration shape:

```yaml
services:
  reports:
    target: http://127.0.0.1:8000
    description: Reporting API
```

Only loopback targets are accepted. Start a server and client with:

```text
filebridge server --share \\server\share\FileBridge --config services.yaml
filebridge client --share \\server\share\FileBridge --client-id employee-a
```

The client listens at `127.0.0.1:9000` by default. Its bridge endpoints are
`/_bridge/status`, `/_bridge/services`, `/_bridge/version`, and
`/_bridge/health`. All other requests use `/{service}/{remote-path}`.

The shared-folder layout is:

```text
server/inbox/<client-id>/
clients/<client-id>/inbox/
registry/services.json
server/heartbeat.json
```

## Testing

Run the automated integration suite from the repository root:

```bash
go test ./...
```

It covers the complete file transport flow: GET requests, query strings, JSON
and binary POST bodies, forwarded headers, and preserved upstream error status.

Build the standalone Windows executable with:

```bash
GOOS=windows GOARCH=amd64 GOAMD64=v1 CGO_ENABLED=0 go build -o filebridge.exe ./cmd/filebridge
```

### Manual smoke test

Use a local directory as the shared folder while testing on one machine. In a
real deployment, replace `./test-share` with the corporate share UNC path.
Open three terminals in the repository root.

1. Start a simple local upstream HTTP service:

```bash
python3 -m http.server 8000 --bind 127.0.0.1
```

2. Create `services.yaml`:

```yaml
services:
  reports:
    target: http://127.0.0.1:8000
    description: Local smoke-test service
```

3. Start the bridge server:

```bash
go run ./cmd/filebridge server --share ./test-share --config services.yaml
```

4. Start the bridge client in a second terminal:

```bash
go run ./cmd/filebridge client --share ./test-share --client-id manual-test
```

5. Verify bridge health and service discovery, then proxy a request:

```bash
curl -i http://127.0.0.1:9000/_bridge/status
curl -i http://127.0.0.1:9000/_bridge/services
curl -i http://127.0.0.1:9000/_bridge/health
curl -i "http://127.0.0.1:9000/reports/?check=1"
```

Expected results: status reports `server_alive: true`, the services endpoint
lists `reports`, health returns HTTP 200, and the final request returns the
upstream directory response through FileBridge. Stop the server or client and
retry the last request to confirm the client returns a bridge-level HTTP 502
rather than hanging.

## Deployment guide

For a complete explanation of the client/server roles, shared-folder
permissions, `services.yaml`, startup commands, HTTP routing, and
troubleshooting, see [Operating FileBridge](docs/USAGE.md).
