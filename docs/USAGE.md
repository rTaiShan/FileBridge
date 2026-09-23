# Operating FileBridge

FileBridge is a small HTTP bridge for a network where employee computers and a
server cannot communicate directly, but both can use the same network share.
It is not a general-purpose proxy: the server can forward only to services it
explicitly registers, and those targets must be bound to loopback.

## Roles and traffic flow

Run one **server** process on the computer that can reach the real HTTP
services. Run one **client** process on every employee computer that needs to
call them.

```text
application -> 127.0.0.1:9000 -> shared folder -> FileBridge server -> 127.0.0.1:<service port>
application <- 127.0.0.1:9000 <- shared folder <- FileBridge server <- 127.0.0.1:<service port>
```

The client writes a request metadata file and a separate raw body file. The
server reads it, calls the registered local service, and writes a separate
response metadata/body pair. Neither side requires inbound network access from
the other.

## Prepare the shared folder

Choose one network share, for example `\\fileserver\FileBridge` on Windows or
`/mnt/filebridge` on Linux. Pass the local path to that share to `--share` on
each machine. The mount paths may differ; they only need to refer to the same
underlying share.

FileBridge creates this layout automatically:

```text
FileBridge/
  server/inbox/<client-id>/       # client writes requests; server reads
  clients/<client-id>/inbox/      # server writes responses; client reads
  registry/services.json          # server writes; clients read
  server/heartbeat.json           # server writes; clients read
```

For a client named `alice-laptop`, grant these permissions:

| Location | Client | Server |
| --- | --- | --- |
| `server/inbox/alice-laptop/` | create, write, delete own requests | read |
| `clients/alice-laptop/inbox/` | read, delete own responses | create, write |
| `registry/`, `server/heartbeat.json` | read | create, write |

The server requires read access to every client request directory and write
access to every response directory. For a first local test, a normal writable
directory is sufficient. The client uses port 9000 on loopback, so it does not
need administrator privileges.

## Configure server services

Create a `services.yaml` on the server machine. The server supports JSON too,
but this YAML form is the intended human-editable format:

```yaml
services:
  reports:
    target: http://127.0.0.1:8000
    description: Reporting API
  dashboard:
    target: http://127.0.0.1:3000
    description: Internal dashboard
  automation:
    target: http://127.0.0.1:5678
    description: Automation service
```

Each service needs:

- a unique service name containing only letters, numbers, `.`, `_`, or `-`;
- a `target` HTTP or HTTPS URL on `localhost`, `127.0.0.1`, or `::1`;
- an optional `description` published to clients.

Target URLs are deliberately restricted to loopback. A client cannot select a
host or port through its HTTP request. Restart the server after changing this
file so it republishes the service registry.

## Start the bridge

Start the server first, on the service-hosting machine:

```bash
filebridge server \
  --share /mnt/filebridge \
  --config /etc/filebridge/services.yaml
```

On a Windows share, the equivalent is:

```text
filebridge.exe server --share \\fileserver\FileBridge --config C:\FileBridge\services.yaml
```

Then start a client on an employee computer:

```bash
filebridge client \
  --share /mnt/filebridge \
  --client-id alice-laptop \
  --listen 127.0.0.1:9000 \
  --timeout 15s
```

Use a stable, unique `--client-id` per installation. If it is omitted,
FileBridge generates and persists one in the operating system configuration
directory. Explicit IDs make share permissions and troubleshooting clearer.

Both roles poll every 500 ms by default. Use `--poll 1s` if lower share traffic
is more important than latency. The client returns a timeout error after 15
seconds by default; change it with `--timeout` when an upstream operation is
expected to take longer.

## Call services through the client

Applications always call the local client, never an upstream port directly.
The first path segment is the configured service name and is removed before
forwarding.

```text
http://127.0.0.1:9000/{service}/{remote-path}?{query}
```

Examples:

```bash
curl http://127.0.0.1:9000/reports/status
curl -X POST http://127.0.0.1:9000/reports/generate \
  -H 'Content-Type: application/json' \
  -d '{"month":"2026-09"}'
curl -X POST --data-binary @invoice.pdf \
  http://127.0.0.1:9000/automation/import
```

`/reports/status` is forwarded to `http://127.0.0.1:8000/status` in the
example configuration. Methods, escaped paths, query strings, normal HTTP
headers, and raw bodies are forwarded. Response status, normal headers, and
raw bodies are returned to the caller. Hop-by-hop transport headers are not
forwarded.

## Inspect bridge state

These endpoints belong to FileBridge and do not reach registered services:

```bash
curl -i http://127.0.0.1:9000/_bridge/status
curl -i http://127.0.0.1:9000/_bridge/services
curl -i http://127.0.0.1:9000/_bridge/version
curl -i http://127.0.0.1:9000/_bridge/health
```

`/_bridge/status` includes the client ID and whether the server heartbeat is
fresh. `/_bridge/services` shows the sanitized service registry; it never
reveals target URLs. `/_bridge/health` returns 200 only while the server
heartbeat is current.

## Error behavior

| Result | Meaning |
| --- | --- |
| 404 | The requested service is absent from the registry. |
| 502 | The registry or heartbeat is unavailable, or the server cannot reach its upstream service. |
| 504 | The client request deadline expired. |
| Any upstream HTTP status | The upstream service answered; its status is preserved. |

If a request returns 502, first call `/_bridge/status`. A false
`server_alive` value normally means the share is unavailable, the server is
stopped, or the server cannot update `server/heartbeat.json`. If health is
good but a specific service fails, check that its configured target is running
on the server machine and is listening on the loopback address/port in
`services.yaml`.
