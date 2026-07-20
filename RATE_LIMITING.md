# Rate limiting (HTTP 429) handling

This client ships with an integrated handler for `429 Too Many Requests`
responses. It performs **exponential backoff with full jitter** and is wired in
at client-instantiation time as an `http.RoundTripper`, so it transparently
covers every endpoint without any per-call boilerplate.

The implementation lives in [`rate_limit.go`](./rate_limit.go) and is *not*
generated code — it survives regeneration (see `.openapi-generator-ignore`).

## Quick start

```go
cfg := pipedriveapi.NewConfiguration()
cfg.EnableRateLimiting()                 // sensible defaults
client := pipedriveapi.NewAPIClient(cfg)
```

or as a single call:

```go
client := pipedriveapi.NewAPIClientWithRateLimiting(pipedriveapi.NewConfiguration())
```

## Tuning

Start from `DefaultRateLimitConfig()` and adjust what you need:

```go
rl := pipedriveapi.DefaultRateLimitConfig()
rl.MaxRetries = 6
rl.BaseDelay  = 250 * time.Millisecond
rl.MaxDelay   = 30 * time.Second
rl.OnRetry = func(attempt int, delay time.Duration, resp *http.Response) {
    log.Printf("rate limited; retry %d in %s", attempt+1, delay)
}

cfg := pipedriveapi.NewConfiguration()
cfg.EnableRateLimiting(rl)
client := pipedriveapi.NewAPIClient(cfg)
```

### Defaults

| Field                  | Default            | Meaning                                                    |
| ---------------------- | ------------------ | ---------------------------------------------------------- |
| `MaxRetries`           | `4`                | Retries after the initial attempt (`0` disables retrying). |
| `BaseDelay`            | `500ms`            | Backoff base; unjittered delay for retry `n` is `Base*2^n`.|
| `MaxDelay`             | `60s`              | Upper bound on any single wait.                            |
| `RetryableStatusCodes` | `[429]`            | Status codes that trigger a retry.                         |
| `RespectRetryAfter`    | `true`             | Honour a `Retry-After` header (seconds or HTTP date).      |
| `OnRetry`              | `nil`              | Optional observability hook fired before each wait.        |

## Behaviour notes

- **Full jitter**: each wait is a uniformly random duration in `[0, min(MaxDelay, BaseDelay*2^attempt)]`, which avoids synchronised retry storms across concurrent clients.
- **`Retry-After`**: when present and enabled, it takes precedence over the computed backoff, still bounded by `MaxDelay`.
- **Context aware**: waits respect the request `context.Context`; a cancelled or timed-out context aborts retrying immediately.
- **Request bodies** are safely replayed across retries (via `http.Request.GetBody`, which the generated client always populates).
- **Transport-level errors** (timeouts, connection resets) are surfaced to the caller unchanged — only rate-limited HTTP responses are retried.
- The global `http.DefaultClient` is never mutated; a nil `HTTPClient` is replaced with a dedicated one.
