# RGB consignment gateway

An HTTP JSON-RPC transport server implementing [RGB HTTP JSON-RPC protocol
v0.2](https://github.com/RGB-Tools/rgb-http-json-rpc/tree/ed5de7627cb4). It lets
standard RGB wallets exchange consignments through a public endpoint.

The gateway is transport-only: it holds no keys, performs no RGB validation,
talks to no chain, and treats consignment bytes as opaque. Clients remain
responsible for validating consignments and broadcasting transactions.

## Protocol

- Endpoint: `POST <path>` (default `/0.2/json-rpc`), one JSON-RPC 2.0 call per
  request.
- `Content-Type: application/json` for ordinary calls.
- `Content-Type: multipart/form-data` for uploads; the file is the `file` part.
  Request fields may be sent as `method`, `jsonrpc`, `id`, `params` (a JSON
  object or a JSON-encoded string), or as flattened `params[key]` fields.
- Batch requests are rejected with `-32600`.

| Method | Behaviour |
| --- | --- |
| `server.info` | Returns `protocol_version: "0.2"`, the server version and uptime in seconds. |
| `consignment.post` | Stores the uploaded bytes. First upload returns `true`; the identical file returns `false`; a different file for an existing `recipient_id` returns `-101`. |
| `consignment.get` | Returns the oldest not-yet-ACKed consignment for the recipient as `{consignment, txid, vout?}` (base64); `-400` if none. Several consignments may share one `recipient_id` (address-based invoices do); they are delivered in order, and an ACK consumes the one returned. |
| `ack.post` | Records an ACK/NACK. First post returns `true`; the same value returns `false`; a different value returns `-100`; no consignment returns `-300`. |
| `ack.get` | Returns `true`, `false`, or `null` (no ACK yet); `-400` if the consignment is absent. |
| `media.post`, `media.get` | Not implemented: returns `-32601`. No media storage is provided. |

## Deposit requests

Besides the standard transport the gateway brokers signed deposit requests, so the bridge never
has to listen for inbound traffic. This API is outside the RGB v0.2 protocol.

| Endpoint | Auth | Behaviour |
| --- | --- | --- |
| `POST /deposit/requests` | public | Accepts `{request_id, chain33_addr, asset_symbol, amount, timestamp, pubkey, signature}`, verifies the signature over the canonical message (chain title, request id, address, asset, amount, timestamp) and stores it idempotently by `request_id`. |
| `GET /deposit/requests/{id}` | public | Returns the request; once the bridge has created it, `status` is `invoiced` and `invoice` / `receive_id` / `recipient_id` are filled in. |
| `GET /bridge/deposit/requests/stream` | bearer | Server-Sent Events stream (one `data:` JSON event per pending request). Replays every request that has no invoice yet on connect, then pushes new ones; `: ping` comments keep the connection alive. |
| `POST /bridge/deposit/invoices` | bearer | Attaches `{request_id, invoice, receive_id, recipient_id}` to the request. A different invoice for an already-invoiced request is a `409`. |

The bridge authenticates `/bridge/*` with `-bridge-token`. When the gateway terminates TLS with
`-client-ca`, those endpoints additionally require a verified client certificate (mTLS); the token
is still checked. `-chain-title` is required for the deposit API: it is part of the signed message
and must equal the bridge's chain title, which is also the rgbx executor's
`guardianParachainTitle` (the parachain title, not the title of the chain hosting the contract).

Protocol errors use the JSON-RPC error object and the v0.2 codes:

| Code | Meaning |
| --- | --- |
| `-100` | Cannot change ACK |
| `-101` | Cannot change uploaded file |
| `-200` | Invalid ACK |
| `-202` | Invalid recipient ID |
| `-203` | Invalid TXID |
| `-204` | Invalid vout |
| `-300` | Missing ACK (also: no consignment for `ack.post`) |
| `-302` | Missing recipient ID |
| `-303` | Missing file |
| `-304` | Missing TXID |
| `-400` | Consignment file not found |

Validation rules:

- `recipient_id` is a non-empty string of at most 256 bytes.
- `txid` is exactly 64 hexadecimal characters.
- `vout`, when present, is a non-negative JSON integer.
- The file must be non-empty and no larger than `--max-size`.

Requests that exceed the size limit return HTTP `413` with a `-32600` error
body. Rate-limited requests return HTTP `429`.

## Incompatibility with protocol 1.0

This server implements **protocol 0.2 only**. It is not compatible with RGB
HTTP JSON-RPC 1.0: the method set, parameters, ACK authentication, sender
proxy, transfer IDs and encryption all differ. A 1.0 wallet cannot use this
endpoint, and no 0.2 data is migrated by an upgrade.

## Build and run

```sh
go build ./plugin/dapp/lightclient/rpc/lightclient/neutrino/cmd/rgb-consignment-gateway
./rgb-consignment-gateway \
  --listen 127.0.0.1:8080 \
  --data-dir /var/lib/rgb-consignment-gateway
```

The effective configuration is printed at startup.

## Configuration

Every option can be set by flag or environment variable. Flags take precedence.

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--listen` | `RGB_GATEWAY_LISTEN` | `127.0.0.1:8080` | TCP listen address. Bind to loopback unless a reverse proxy fronts the service. |
| `--data-dir` | `RGB_GATEWAY_DATA_DIR` | `rgb-consignment-data` | Directory for blobs, index records and temporary files. Created with mode `0700`. |
| `--path` | `RGB_GATEWAY_PATH` | `/0.2/json-rpc` | HTTP path of the endpoint. Must start with `/`. |
| `--max-size` | `RGB_GATEWAY_MAX_SIZE` | `16777216` (16 MiB) | Maximum consignment size in bytes. |
| `--ttl` | `RGB_GATEWAY_TTL` | `168h` (7 days) | Consignment retention. Expired entries are removed on startup and periodically afterwards. |
| `--rate-limit` | `RGB_GATEWAY_RATE_LIMIT` | `60` | Requests per client IP per minute. `0` disables limiting. |
| `--trust-proxy` | `RGB_GATEWAY_TRUST_PROXY` | `false` | Take the client IP from `X-Forwarded-For`/`X-Real-IP`. Enable only when a trusted reverse proxy sets these headers. |

Durations use Go syntax (`168h`, `30m`); sizes are plain bytes.

## Deployment

TLS is not implemented by the gateway. Terminate TLS in a reverse proxy (nginx,
Caddy, HAProxy) that forwards to the configured loopback address, and publish
only the HTTPS listener. Example nginx location:

```nginx
location /0.2/json-rpc {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Real-IP $remote_addr;
    client_max_body_size 20m;
}
```

If the proxy sets those headers, start the gateway with `--trust-proxy`.
Without it, all proxied requests share the proxy's address for rate limiting.
The reverse proxy should also cap request bodies and concurrent connections;
the gateway limits each file to `--max-size` but buffers that file in memory.

The server handles `SIGINT`/`SIGTERM` with a graceful shutdown.

## Storage and retention

- Consignment bytes are stored once, addressed by their SHA-256 under
  `<data-dir>/blobs/`.
- `<data-dir>/index/<sha256(recipient_id)>.json` maps a recipient ID to the
  blob hash, `txid`, optional `vout`, creation time and ACK state.
- Files are written to `<data-dir>/tmp/`, fsynced, renamed into place, and the
  containing directory is fsynced. A crash leaves either the old state or the
  complete new state.
- Entries older than `--ttl` are ignored immediately and deleted by cleanup,
  which runs once at startup and then every `min(ttl/4, 1h)` (at least 1s).
  ACKs are deleted with their entry; blobs not referenced by any live entry
  are removed.
- The store is rebuilt from the index at startup, so restarts preserve
  consignments, ACK state and idempotency.
- A recipient ID is immutable for its lifetime: a different file or a changed
  ACK is rejected. After the TTL expires the ID can be reused.

## Privacy and security

- No authentication: the protocol has none. Protection comes from per-IP rate
  limiting, the file size limit and the retention TTL.
- Consignment bytes are never logged. Access logs contain the method, the
  first eight characters of `recipient_id` (the rest is masked), the result and
  the duration. `txid` is not logged; when it is, remember that it is public
  chain data.
- The gateway does not inspect or validate consignment contents and does not
  publish them anywhere.
- Files are kept for at most `--ttl` and then deleted; deployments subject to
  data-protection rules should set a TTL that matches their retention policy.

## Tests

```sh
gofmt -l ./plugin/dapp/lightclient/rpc/lightclient/neutrino/cmd/rgb-consignment-gateway
go vet ./plugin/dapp/lightclient/rpc/lightclient/neutrino/cmd/rgb-consignment-gateway
go test -count=1 ./plugin/dapp/lightclient/rpc/lightclient/neutrino/cmd/rgb-consignment-gateway
```

The suite covers idempotent and conflicting uploads, get round-trips, the three
ACK states and their error codes, parameter validation, size limits, rate
limiting (including trusted-proxy mode), TTL expiry and cleanup, restart
recovery, content-addressed deduplication, batch rejection and log masking.
