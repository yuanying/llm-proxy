# llm-proxy

A pass-through proxy that serves one ChatGPT subscription to trusted callers on the OpenAI Responses API.

One process holds the "Sign in with ChatGPT" token and refreshes it. Callers send ordinary Responses API requests to the proxy; the proxy swaps their `Authorization` for the real access token and forwards the request to `https://api.openai.com/v1` unchanged. Streams are passed through as they arrive. Nothing in the request or response body is read or rewritten, so reasoning items and prompt cache keys sent by the caller reach the API.

The proxy is meant to sit behind [gated](https://github.com/yuanying/gated), which authenticates callers (for example by their Kubernetes ServiceAccount token) and tells the proxy who they are with a signed `Gated-Assertion` header. The proxy checks that assertion on every request, so requests that reach it without going through gated are refused.

Design decisions are in [`docs/adr/`](docs/adr/).

## What passes

| | |
|---|---|
| Routes | `POST /v1/responses` and `GET /v1/models` by default (exact paths). Other paths get 404, other methods 405 |
| Request headers changed | `Authorization` is replaced with the real token. `Cookie`, `Gated-*`, `Forwarded`, `X-Forwarded-*`, `X-Real-IP` and hop-by-hop headers are dropped. Everything else passes |
| Response | Passed back as it is, status, headers and body, compressed or not. Every write is flushed at once |
| Refusals | 401 without a valid assertion, 403 for a subject not allowed, 503 when there is no usable token, 502 when the upstream does not answer |
| `GET /healthz` | 200, without an assertion or a token |

## Commands

`llm-proxy serve` runs the proxy. `llm-proxy login` signs in and writes the token file. `llm-proxy version` prints the version.

Every flag can also be set with an environment variable: `LLM_PROXY_` followed by the flag name in upper case with `-` as `_` (for example `--data-dir` is `LLM_PROXY_DATA_DIR`). A flag on the command line wins.

### `serve`

| Flag | Default | |
|---|---|---|
| `--data-dir` | (required) | Directory holding `auth.json`. Only one `serve` may use a directory at a time; a second one refuses to start |
| `--listen` | `:8080` | Address to listen on |
| `--upstream` | `https://api.openai.com/v1` | The URL that stands for `/v1` |
| `--routes` | `POST /v1/responses,GET /v1/models` | Comma-separated `METHOD /v1/path` entries that pass |
| `--upstream-header-timeout` | `10m` | How long the upstream may take to send the response headers. The body has no limit |
| `--refresh-before` | `10m` | How long before expiry the access token is refreshed |
| `--gated-issuer` | (required) | gated's central authentication host, as an `https://` origin |
| `--gated-jwks-url` | `<gated-issuer>/__gated/jwks.json` | Where gated publishes its keys |
| `--gated-audience` | (required) | The proxy's public host name, as gated writes it in the assertion's `aud` |
| `--allowed-subjects` | (required) | Comma-separated subjects let through, such as `system:serviceaccount:<namespace>:<name>` |
| `--insecure-skip-assertion` | `false` | Do not check the assertion. Anyone who reaches the proxy can then use the subscription. Cannot be combined with the `--gated-*` flags |
| `--oauth-issuer` | `https://auth.openai.com` | Origin of the OAuth token endpoint |

Without a token file the proxy starts and answers 503 until a login writes one. The token is refreshed before requests and by a check every minute. A login written while the proxy runs is picked up without a restart.

Logs are JSON lines on standard error. Each request logs its method, path (without the query), status, duration and the subject from the assertion. Tokens, assertions, headers and bodies are never logged.

### `login`

| Flag | Default | |
|---|---|---|
| `--data-dir` | (required) | The same directory as `serve` |
| `--agent-name` | `llm-proxy` | The name the login introduces itself with |
| `--callback-listen` | `127.0.0.1:1455` | Where to listen for the browser's callback. Empty to only take a pasted URL |
| `--timeout` | `10m` | How long to wait |
| `--oauth-issuer` | `https://auth.openai.com` | Origin of the OAuth endpoints |

The login prints a URL to open in a browser. After you approve, the browser is sent to `http://127.0.0.1:1455/auth/callback?...`. Hand that back in one of two ways:

- Paste it. The page does not open, but the address bar holds the whole URL. Copy it and paste it into the terminal running `llm-proxy login`.
- Forward the port. Start `kubectl port-forward <pod> 1455:1455` before logging in, and the browser delivers the callback by itself.

The login writes `auth.json` in the data directory and prints where. The installation's ID is kept in `device-id` beside it, so a later login is the same installation.

## Running in Kubernetes

- One replica, with the `Recreate` strategy: two processes refreshing the same token cut each other off.
- The data directory on a persistent volume, writable by uid 65532 (the image runs as distroless's `nonroot`).
- An Ingress served by gated, with the callers' subjects in both gated's `NetworkRoleBinding` and `--allowed-subjects`. `--gated-audience` is the Ingress's host name.
- To log in: `kubectl exec -it <pod> -- /llm-proxy login --data-dir <the data directory>` (or set `LLM_PROXY_DATA_DIR` on the container and leave the flag out).
- Probes on `GET /healthz`.

The image is `ghcr.io/yuanying/llm-proxy:<version>` for linux/amd64 and linux/arm64. Its default command is `serve`.

## Development

`make help` lists the targets. `make test` needs nothing but the Go toolchain; the OpenAI API, the OAuth endpoints and gated are fakes in the tests.

## License

MIT
