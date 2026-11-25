# Grand Blue Lore

<p align="center">
    <img alt="Go Badge" src="https://img.shields.io/badge/Go-2b7d9c?logo=go&logoColor=fff&style=flat"/>
    <img alt="License Badge" src="https://img.shields.io/github/license/gblgo/gbl"/>
</p>
<p align="center">
    <img alt="GitHub Repo stars" src="https://img.shields.io/github/stars/gblgo/gbl.svg">
    <img alt="GitHub forks" src="https://img.shields.io/github/forks/gblgo/gbl.svg">
</p>

<p align="center">
  <a href="https://lutme.github.io/gblWeb/#/docs/introduction"><b>Official Docs</b></a> ·
  <a href="README.md">English</a> ·
  <a href="README_zh-CN.md">简体中文</a>
</p>

<p align="center">
  <b>gbl</b> is a <b>zero-dependency</b> Go web framework built entirely on the
  standard library. Its core idea is simple: <b>write a handler with any supported
  signature, and gbl binds its parameters and serializes its return value.</b>
</p>

> **Looking for details?** This README is a quick overview. Complete bilingual
> guides, edge cases and examples live in the
> [official documentation](https://lutme.github.io/gblWeb/#/docs/introduction).
> Start with [Getting Started](https://lutme.github.io/gblWeb/#/docs/getting-started)
> or browse the [API Reference](https://lutme.github.io/gblWeb/#/docs/api-reference).

---

## Quick start

Requires **Go 1.27+**.

```bash
go get github.com/gblgo/gbl
```

```go
package main

import "github.com/gblgo/gbl"

func main() {
    gbl.Get("/hello", func() string {
        return "hello, gbl"
    })

    gbl.Run() // listens on :9801 by default
}
```

```bash
go run .
# GET http://localhost:9801/hello -> hello, gbl
```

## What you get

| Area | Included |
|---|---|
| Routing | Seven HTTP verbs, static-first matching, path parameters, groups, HEAD/OPTIONS fallbacks |
| Binding | Path, query, form, JSON and multipart values bound by type and name |
| Handlers | Automatic text/JSON serialization, validation and raw response access |
| Middleware | Onion chain with `Next`, group scope and `Abort` |
| Streaming | Built-in GET SSE with heartbeats, cancellation and lifecycle callbacks |
| Safety | Body/upload limits, CORS policy, gzip middleware and fail-fast startup checks |
| Operations | Pluggable logging, TLS, timeouts and graceful shutdown |
| Performance | Precomputed binding plans, pooled request objects and static-route map lookups |

Detailed guides are available for
[handlers](https://lutme.github.io/gblWeb/#/docs/handlers),
[routing](https://lutme.github.io/gblWeb/#/docs/routing),
[middleware](https://lutme.github.io/gblWeb/#/docs/middleware),
[SSE](https://lutme.github.io/gblWeb/#/docs/sse) and
[deployment](https://lutme.github.io/gblWeb/#/docs/deployment).

## Handlers and binding

A handler is a non-nil function with **at most one return value**. Variadic
functions, method values, method expressions and generic instantiations are not
supported; wrap a method value or expression in a closure when needed. Name-bound
handlers must start on distinct source lines; ambiguous positions fail startup.

### Injected parameters

| Parameter | What gbl supplies |
|---|---|
| `*gbl.Context` | Request context and middleware helpers |
| `*http.Request` | Inbound request |
| `http.ResponseWriter` | Wrapped writer for direct response control |
| `gbl.SSEWriter` | SSE stream on GET routes |
| `multipart.FileHeader` / `[]multipart.FileHeader` | Uploaded files, including pointer forms |
| Any other type | Business value bound by parameter or field name |

### Binding essentials

- A same-named **path parameter wins** over every other source.
- Without a path hit, `Content-Type` selects the source: non-empty JSON uses the
  body; other content types use form/query values. On body methods, form fields
  override same-named query values.
- Only `POST`, `PUT` and `PATCH` read request bodies. Other methods use path/query
  parameters only.
- One business parameter receives the whole JSON body. Multiple business
  parameters read same-named top-level members; a missing member becomes its zero
  value and then goes through `Validatable`, if implemented.
- Form/query names match exactly. Struct fields use their JSON tag, or the field
  name with its first letter lowercased (`Name` becomes `name`). Prefer `Id` over
  `ID` when relying on this convention.

```go
type User struct {
    Id   int
    Name string
}

gbl.Get("/user/:id", func(id int, verbose bool) string {
    return fmt.Sprintf("user %d verbose=%v", id, verbose)
})

gbl.Post("/user", func(u User) *User {
    u.Name = strings.ToUpper(u.Name)
    return &u
})
```

### Return values

| Return type | Response |
|---|---|
| `string`, `[]byte`, bool, numbers, complex values | Plain text |
| Struct, map, array, non-byte slice, pointer to a composite | Usually JSON; `error` and nil values follow runtime conversion |
| Interface | Runtime value: plain text or JSON; nil is empty |
| Pointer to a scalar | Plain text |
| `error` | Its message as plain text |
| Nil pointer/interface or no return value | Empty body |

For return-value handlers, gbl sets a provisional `Content-Type` before
middleware and the handler run, so `Flush` commits a usable type. Runtime
serialization may refine it before the automatic write when headers are not yet
committed.

See the full [binding guide](https://lutme.github.io/gblWeb/#/docs/handlers) for
pointer chains, duplicate values, arrays, naming rules and JSON edge cases.

## Routes, groups and middleware

```go
gbl.Get("/items/:id", func(id int) string { return strconv.Itoa(id) })
gbl.Post("/items", func(item Item) Item { return item })
gbl.Delete("/items/:id", func(id int) { deleteItem(id) })

api := gbl.Group("/api")
api.Get("/ping", func() string { return "pong" })

gbl.Use(func(c *gbl.Context) {
    start := time.Now()
    c.Next()
    gbl.Info("request", "path", c.Request.URL.Path,
        "status", c.StatusCode(), "cost", time.Since(start))
})
```

- Static routes take precedence over dynamic routes. Repeated or trailing `/`
  separators are normalized for matching. Overlapping dynamic patterns are
  rejected at startup.
- Unregistered `HEAD` reuses the matching `GET` route. Unregistered `OPTIONS`
  returns `204` with an `Allow` header.
- `c.Next()` runs the remaining chain; code after it runs while the chain unwinds.
- `Abort` and framework binding/validation errors stop downstream execution.
  Upstream post-`Next()` code still runs.
- Install `GroupUse` before registering routes in that group.

## SSE and raw responses

Declare `gbl.SSEWriter` on a GET handler to stream events. SSE handlers must not
return values; use `Send` and stop when `Done` closes.

```go
gbl.Get("/events", func(s gbl.SSEWriter) {
    ticker := time.NewTicker(time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-s.Done():
            return
        case <-ticker.C:
            if err := s.Send(map[string]int{"tick": 1}); err != nil {
                return
            }
        }
    }
})
```

gbl manages SSE headers, flushing, idle heartbeats, write deadlines and
cancellation. Optional `OnError` and `OnComplete` callbacks cover stream failures
and termination; they run asynchronously after the stream has terminated and its
in-flight writes have drained.

For direct HTTP control, inject `http.ResponseWriter`. Standard optional
interfaces and `http.ResponseController` remain available. A direct `Write` or
`Hijack` prevents automatic return-value writeback; hijacked connections become
the application's responsibility and are closed after successful framework
shutdown.

Read the [SSE guide](https://lutme.github.io/gblWeb/#/docs/sse) and
[ResponseWriter guide](https://lutme.github.io/gblWeb/#/docs/response-writer) for
callbacks, 1xx/101 responses, deadlines, Hijack and WebSocket integration.

## Validation and logging

Implement `Validate() error` on a business parameter to run validation after
binding and before the handler. A validation error returns `400` with your error
text. Nil pointers are passed through without calling `Validate`.

```go
func (u User) Validate() error {
    if u.Name == "" {
        return errors.New("name is required")
    }
    return nil
}
```

`Logger` is the single logging contract for framework events and `Info`, `Warn`
and `Error`. The default writes dated lines to a private console logger, with
level colors on a real terminal; call `SetLogger` before `Run` to use your own
backend.

## Limits, configuration and shutdown

| Setting | Default |
|---|---|
| Port | `9801` |
| Total request body | 5 MiB |
| Multipart memory threshold | 2 MiB |
| Individual uploaded file | 1 MiB |
| Signal shutdown window | 5 seconds |

`SetMaxFileSize` is a memory/disk spill threshold, not an upload cap.
`SetMaxBodySize` and `SetMaxUploadFileSize` enforce hard limits.

```go
gbl.Configure(func(c *gbl.Config) {
    c.Port = "8080"
    c.WriteTimeout = 30 * time.Second
})

gbl.Get("/ready", func() string { return "ok" })
gbl.Run()
```

All registration and configuration APIs must run **before `Run`**. `Run` handles
`SIGINT`/`SIGTERM`; call `Shutdown(ctx)` to trigger graceful shutdown yourself.
Shutdown cancels SSE streams, drains active requests and closes tracked hijacked
connections after success.

## Building and deploying

A normal production build works without extra tooling:

```bash
go build -trimpath -o app ./cmd/api
```

Name-based binding recovers parameter names from source or the binary's DWARF
data. Avoid `-ldflags "-w"` and `"-s"` unless every name-bound handler uses a
self-describing struct parameter. See
[Deployment & Builds](https://lutme.github.io/gblWeb/#/docs/deployment).

## License

[MIT](./LICENSE) © lutme
