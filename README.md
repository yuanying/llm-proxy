# llm-proxy

A pass-through proxy that serves one ChatGPT or xAI subscription to trusted callers on the Responses API. With xAI, callers can use tools such as `x_search` on a SuperGrok or X Premium subscription without holding its OAuth tokens.

Each proxy process holds and refreshes the token for one provider. The proxy replaces the caller's `Authorization` with the access token and forwards the request. ChatGPT is the default and keeps its existing request adjustment (see below). In xAI mode, the proxy forwards request bodies byte for byte. Response bodies are streamed through in both modes.

The proxy is meant to sit behind [gated](https://github.com/yuanying/gated), which authenticates callers (for example by their Kubernetes ServiceAccount token) and tells the proxy who they are with a signed `Gated-Assertion` header. The proxy checks that assertion on every request, so requests that reach it without going through gated are refused.

Design decisions are in [`docs/adr/`](docs/adr/).

## What passes

| | |
|---|---|
| Routes | `POST /v1/responses` and `GET /v1/models` by default (exact paths). Other paths get 404, other methods 405 |
| Request headers changed | `Authorization` is replaced with the real token. `Cookie`, `Gated-*`, `Forwarded`, `X-Forwarded-*`, `X-Real-IP` and hop-by-hop headers are dropped. Everything else passes |
| `POST /v1/responses` body (OpenAI mode) | Read in full (up to 64 MiB; larger gets 413) and sent with the parts the subscription refuses taken out: the `configuration_update` input items and the top-level `prompt_cache_retention`, `prompt_cache_options`, `max_output_tokens` and `temperature`. When a `configuration_update` carried `reasoning.effort`, the last one's effort becomes the request's `reasoning.effort`. Everything else is kept, and a body with nothing to take out is sent byte for byte. A body that is not one JSON object, or that has a `Content-Encoding`, is sent as it is |
| Other request bodies, including all requests in xAI mode | Streamed through unread |
| Response | Passed back as it is, status, headers and body, compressed or not. Every write is flushed at once |
| Refusals | 401 without a valid assertion, 403 for a subject not allowed, 503 when there is no usable token, 502 when the upstream does not answer |
| `GET /healthz` | 200, without an assertion or a token |

## Commands

`llm-proxy serve` runs the proxy. `llm-proxy login` signs in and writes the token file. `llm-proxy version` prints the version.

Every flag can also be set with an environment variable: `LLM_PROXY_` followed by the flag name in upper case with `-` as `_` (for example `--data-dir` is `LLM_PROXY_DATA_DIR`). A flag on the command line wins.

### `serve`

| Flag | Default | |
|---|---|---|
| `--provider` | `openai` | `openai` for ChatGPT or `xai` for SuperGrok / X Premium |
| `--data-dir` | (required) | Directory holding `auth.json`. Use a separate directory for each provider. Only one `serve` may use a directory at a time |
| `--listen` | `:8080` | Address to listen on |
| `--upstream` | `https://api.openai.com/v1` or `https://api.x.ai/v1` | The URL that stands for `/v1`, selected by provider unless overridden |
| `--routes` | `POST /v1/responses,GET /v1/models` | Comma-separated `METHOD /v1/path` entries that pass |
| `--upstream-header-timeout` | `10m` | How long the upstream may take to send the response headers. The body has no limit |
| `--refresh-before` | `10m` | How long before expiry the access token is refreshed |
| `--gated-issuer` | (required) | gated's central authentication host, as an `https://` origin |
| `--gated-jwks-url` | `<gated-issuer>/__gated/jwks.json` | Where gated publishes its keys |
| `--gated-audience` | (required) | The proxy's public host name, as gated writes it in the assertion's `aud` |
| `--allowed-subjects` | (required) | Comma-separated subjects let through, such as `system:serviceaccount:<namespace>:<name>` |
| `--insecure-skip-assertion` | `false` | Do not check the assertion. Anyone who reaches the proxy can then use the subscription. Cannot be combined with the `--gated-*` flags |
| `--oauth-issuer` | `https://auth.openai.com` or `https://auth.x.ai` | Origin of the selected provider's OAuth token endpoint |

Without a token file for the selected provider, the proxy starts but answers 503 until a matching login writes one. The token is refreshed before requests and by a check every minute. A login written while the proxy runs is picked up without a restart.

Logs are JSON lines on standard error. Each request logs its method, path (without the query), status, duration and the subject from the assertion. A request the proxy took something out of logs one more line with its path, subject, and the names of what was changed: `dropped_fields`, `dropped_input_items` and `moved_fields`. Tokens, assertions, headers, bodies and the values of what was taken out are never logged.

### `login`

| Flag | Default | |
|---|---|---|
| `--provider` | `openai` | Use the same provider as `serve` |
| `--data-dir` | (required) | The same directory as `serve` |
| `--agent-name` | `llm-proxy` | OpenAI only: the name the login introduces itself with |
| `--callback-listen` | `127.0.0.1:1455` | OpenAI only: where to listen for the browser's callback. Empty to only take a pasted URL |
| `--timeout` | `10m` | How long to wait |
| `--oauth-issuer` | `https://auth.openai.com` or `https://auth.x.ai` | Origin of the selected provider's OAuth endpoints |
| `--xai-client-id` | pi's public xAI client ID | xAI only: OAuth public client ID used at login. Its value is saved with the grant for refresh |

#### Sign in with ChatGPT

The login prints a URL to open in a browser. After you approve, the browser is sent to `http://127.0.0.1:1455/auth/callback?...`. Hand that back in one of two ways:

- Paste it. The page does not open, but the address bar holds the whole URL. Copy it and paste it into the terminal running `llm-proxy login`.
- Forward the port. Start `kubectl port-forward <pod> 1455:1455` before logging in, and the browser delivers the callback by itself.

The login writes `auth.json` in the data directory and prints where. The installation's ID is kept in `device-id` beside it, so a later login is the same installation.

#### Sign in with SuperGrok or X Premium

Run `llm-proxy login --provider xai --data-dir <the xAI data directory>` where the token will be used. Open the URL it prints, enter the displayed user code, and approve the login. No callback or port-forward is needed. The command waits for approval and writes `auth.json`; the running xAI proxy picks it up without a restart.

The default routes include `GET /v1/models` because the xAI API exposes it. Subscription access is decided by xAI; the proxy returns any refusal unchanged. Callers can also specify a model ID directly in `POST /v1/responses`.

## Running in Kubernetes

- One replica, with the `Recreate` strategy: two processes refreshing the same token cut each other off.
- The data directory on a persistent volume, writable by uid 65532 (the image runs as distroless's `nonroot`).
- An Ingress served by gated, with the callers' subjects in both gated's `NetworkRoleBinding` and `--allowed-subjects`. `--gated-audience` is the Ingress's host name.
- To log in: `kubectl exec -it <pod> -- /llm-proxy login --data-dir <the data directory>` (or set `LLM_PROXY_DATA_DIR` on the container and leave the flag out).
- To serve xAI alongside ChatGPT, use a separate deployment and persistent data directory. Set `--provider xai` on `serve` and add `--provider xai` to the login command in that Pod, or set `LLM_PROXY_PROVIDER=xai` for both.
- Probes on `GET /healthz`.

The image is `ghcr.io/yuanying/llm-proxy:<version>` for linux/amd64 and linux/arm64. Its default command is `serve`.

## Development

`make help` lists the targets. `make test` needs nothing but the Go toolchain; the upstream APIs, the OAuth endpoints and gated are fakes in the tests.

## License

MIT
