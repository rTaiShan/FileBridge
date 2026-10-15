# Operating FileBridge

FileBridge is a small HTTP bridge for a network where employee computers and a
server cannot communicate directly, but both can use the same network share.
It is not a general-purpose proxy: the server can forward only to services it
explicitly registers, and those targets must be bound to loopback.

## Roles and traffic flow

Run one or more **server** processes on computers that can reach the real HTTP
services. Every server needs a stable, unique server ID. Run one **client** process on every employee computer that needs to call them.

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
  servers/<server-id>/
    inbox/<client-id>/             # client writes only to this server
    registry.json                  # this server publishes its services
    heartbeat.json                 # this server publishes liveness
  clients/<client-id>/responses/<server-id>/ # this server writes responses
```

For a client named `alice-laptop`, grant these permissions:

| Location | Client | Server |
| --- | --- | --- |
| `servers/<server-id>/inbox/alice-laptop/` | create, write, delete own requests | read for that server |
| `clients/alice-laptop/responses/<server-id>/` | read, delete own responses | create, write for that server |
| `servers/<server-id>/registry.json`, `heartbeat.json` | read | create, write only for that server |

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
  --server-id reports-east \
  --config /etc/filebridge/services.yaml
```

On a Windows share, the equivalent is:

```text
filebridge.exe server --share \\fileserver\FileBridge --server-id reports-east --config C:\FileBridge\services.yaml
```

Then start a client on an employee computer:

```bash
filebridge client \
  --share /mnt/filebridge \
  --client-id alice-laptop \
  --listen 127.0.0.1:9000 \
  --timeout 15s \
  --servers reports-east
```

Use a stable, unique `--client-id` per installation. If it is omitted,
FileBridge generates and persists one in the operating system configuration
directory. Explicit IDs make share permissions and troubleshooting clearer.

Both roles poll every 5 seconds by default. Use `--poll 1s` when lower share
latency is more important than reduced share traffic. The client returns a
timeout error after 15 seconds by default; change it with `--timeout` when an
upstream operation is expected to take longer. Shared-folder publication
failures are retried by the server, including transient Windows/SMB rename
failures. If a client times out, it marks the request as cancelled so the
server will not start forwarding it; old request and response files are
reclaimed by the client's refresher.

## Call services through the client

Applications always call the local client, never an upstream port directly.
The explicit route selects a server first; its service segment is removed before
forwarding. The legacy `/{service}/{remote-path}` route is accepted only when exactly one discovered server advertises that service.

```text
http://127.0.0.1:9000/_bridge/proxy/{server-id}/{service}/{remote-path}?{query}
```

Examples:

```bash
curl http://127.0.0.1:9000/_bridge/proxy/reports-east/reports/status
curl -X POST http://127.0.0.1:9000/_bridge/proxy/reports-east/reports/generate \
  -H 'Content-Type: application/json' \
  -d '{"month":"2026-09"}'
curl -X POST --data-binary @invoice.pdf \
  http://127.0.0.1:9000/_bridge/proxy/reports-east/automation/import
```

`/_bridge/proxy/reports-east/reports/status` is forwarded to `http://127.0.0.1:8000/status` in the
example configuration. Methods, escaped paths, query strings, normal HTTP
headers, and raw bodies are forwarded. Response status, normal headers, and
raw bodies are returned to the caller. Hop-by-hop transport headers are not
forwarded.

## Inspect bridge state

These endpoints belong to FileBridge and do not reach registered services:

```bash
curl -i http://127.0.0.1:9000/_bridge/status
curl -i http://127.0.0.1:9000/_bridge/services
curl -i http://127.0.0.1:9000/_bridge/activity
curl -i http://127.0.0.1:9000/_bridge/version
curl -i http://127.0.0.1:9000/_bridge/health
```

For a read-only browser dashboard, open
`http://127.0.0.1:9000/_bridge/ui/`. It shows the local client identity,
connection health, discovered servers, and the services each server exposes.
The dashboard only displays explicitly-published service information; it does
not expose server-local target URLs or change client settings.

`/_bridge/status` includes the client ID, whether the server heartbeat is fresh,
the time the status was checked, and the last successful registry refresh.
`/_bridge/services` shows the sanitized service registry; it never reveals
target URLs. `/_bridge/activity` returns up to 50 recent proxied
request/response metadata records, including method, route, selected server and
service, status, duration, and byte counts. It does not retain headers or
bodies. `/_bridge/health` returns 200 only while the server heartbeat is
current and includes the check time and live-server counts.

## Error behavior

| Result | Meaning |
| --- | --- |
| 404 | The requested service is absent from the registry. |
| 400 | The request metadata or body is invalid or incomplete. |
| 502 | The registry or heartbeat is unavailable, or the server cannot reach its upstream service. |
| 504 | The client request deadline expired. |
| Any upstream HTTP status | The upstream service answered; its status is preserved. |

If a request returns 502, first call `/_bridge/status`. A false
`server_alive` value normally means the share is unavailable, the server is
stopped, or the selected server cannot update its heartbeat. If health is
good but a specific service fails, check that its configured target is running
on the server machine and is listening on the loopback address/port in
`services.yaml`.
