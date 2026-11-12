# Grand Blue Lore
<div align="center">
    <img alt="Go Badge" src="https://img.shields.io/badge/Go-2b7d9c?logo=go&logoColor=fff&style=flat"/>
    <img alt="License Badge" src="https://img.shields.io/github/license/gblgo/gbl"/>
</div>
<div align="center">
    <img alt="GitHub Repo stars" src="https://img.shields.io/github/stars/gblgo/gbl.svg">
    <img alt="GitHub forks" src="https://img.shields.io/github/forks/gblgo/gbl.svg">
    <img alt="GitHub watchers" src="https://img.shields.io/github/watchers/gblgo/gbl.svg">
</div>

<p align="center">
  <a href="README.md">English</a> · <a href="README_zh-CN.md">简体中文</a>
</p>

<p align="center">
  <b>gbl</b> is a <b>zero-dependency</b> Go web framework: build a full HTTP
  service with only the standard library and no third-party packages. Its
  signature feature is — <b>write a handler with any signature and the framework
  binds every argument and serializes the result for you.</b>
</p>

---

## Contents
1. [Features](#1-features)
2. [Quick examples](#2-quick-examples)
3. [Extension points](#3-extension-points)
4. [Configuration (application.ini)](#4-configuration-applicationini)
5. [Install & run](#5-install--run)

---

## 1. Features

| Capability | Description |
|---|---|
| **Zero dependencies** | `go.mod` pulls in no third-party package — standard library only. Small footprint, no supply-chain baggage. |
| **Any-signature handler** | Arguments are bound by **type + name**: path params, query, JSON/form bodies, `*Context`, `*http.Request`, `http.ResponseWriter`, uploaded files. No fixed signature required. |
| **Automatic result serialization** | Just `return` your value: `string`/scalars emit as text, `struct`/`map`/slice auto-encode to JSON, a `nil` emits an empty body. |
| **Routing-tree matching** | Static segments take priority, dynamic (`:id`) segments backtrack; one tree per HTTP method. |
| **Full HTTP method coverage** | `GET / HEAD / POST / PUT / PATCH / DELETE / OPTIONS`, plus `Any` / `Match`. An unregistered `HEAD` borrows `GET`; an unregistered `OPTIONS` is answered with `204 + Allow`. |
| **Onion middleware chain** | Global `Use` + scoped `group.Use`, split before/after `c.Next()`, with post-chain response override. |
| **Route groups** | `Group("/prefix")` for a shared prefix and group-scoped middleware. |
| **Zero-cost validation** | Any bound business parameter implementing `Validate() error` is checked automatically after binding and before the handler. |
| **Safe error semantics** | Unexpected panics always return `500` and **never echo internals** (details go to the log); only an explicit `Abort(status, msg)` is echoed. |
| **Pluggable logger** | Built-in `Info/Warn/Error`; swap in zap/slog via `SetLogger`; per-level file logging supported. |
| **Graceful shutdown** | On `SIGINT/SIGTERM` or a manual `Shutdown(ctx)`, in-flight requests finish within a grace period. |
| **CORS** | Preflight, origin/method/header matching, credentials, `max-age` (`SetCorsConfig` / `UseCors`). |
| **Standard-library compatible** | `Engine` implements `http.Handler`, so it embeds into `net/http` or is driven directly by `httptest`. |

> On performance: the hot path is already optimized with a precomputed bind plan,
> pooled objects (ResponseWriter / path params / Context) and allocation-free
> route matching. The remaining gap versus mature frameworks comes from the
> inherent cost of reflective invocation of arbitrary handler signatures — a
> trade-off of this framework's core feature, not something easily erased.

---

## 2. Quick examples

### Minimal runnable
```go
package main

import "github.com/gblgo/gbl"

func main() {
    gbl.Get("/hello", func() string {
        return "hello, gbl"
    })

    // Listens on :9801 by default; Ctrl+C / kill triggers graceful shutdown
    gbl.Run()
}
```

### Path params / query / JSON body — all bound automatically
```go
type User struct {
    ID   int
    Name string
}

// :id is bound to the parameter named id
gbl.Get("/user/:id", func(id string) string {
    return "user " + id
})

// POST + application/json: the body is decoded straight into User
gbl.Post("/user", func(u User) *User {
    u.Name = strings.ToUpper(u.Name)
    return &u // a struct pointer is returned as JSON automatically
})

// Query values also bind by parameter name (path still wins on a name clash).
// GET /user/42?verbose=true -> id="42" from the path, verbose=true from the query
gbl.Get("/user/:id", func(id string, verbose bool) string {
    return fmt.Sprintf("user %s verbose=%v", id, verbose)
})
```

> **How a single parameter is sourced (highest to lowest):** path parameter
> (matched by the parameter's own name) → JSON body → form/query values; name
> matching is exact first, then case-insensitive.
>
> - A lone struct parameter decodes the whole JSON body in one piece.
> - With a JSON body and **multiple** business parameters, every non-path
>   parameter must appear as a top-level JSON member named like it, e.g.
>   `{"verbose": true, "u": {"name": "ada"}}`.
> - In form/query mode a struct's fields are filled from path and form values by
>   **field name** (case-insensitive), so `:id` can also populate a `u.ID`
>   field — in JSON mode it cannot; there `u` comes only from the body.

### Reach for the raw types when you want full control
```go
import "net/http"

// Declare http.ResponseWriter to own the response entirely
gbl.Get("/raw", func(w http.ResponseWriter) {
    w.WriteHeader(http.StatusCreated)
    _, _ = w.Write([]byte("direct"))
})

// Access the original request
gbl.Post("/echo", func(r *http.Request) string {
    return r.Header.Get("X-Trace-Id")
})
```

### Middleware + groups
```go
// Global middleware: before c.Next() = request phase, after = response phase
gbl.Use(func(c *gbl.Context) {
    c.Set("requestID", newID())
    c.Next()
})

api := gbl.Group("/api")
api.Use(func(c *gbl.Context) {
    if c.Query("token") == "" {
        c.AbortWithStatus(http.StatusUnauthorized) // stop the chain, skip handler
        return
    }
    c.Next()
})
api.Get("/ping", func() string { return "pong" }) // effective path: /api/ping

// One line to install the access-log middleware
gbl.Use(gbl.AccessLog)
```

### In-request values / validation / abort
```go
type Signup struct {
    Email string
}

// Implementing Validate runs it automatically (no nil-check in the handler)
func (s Signup) Validate() error {
    if !strings.Contains(s.Email, "@") {
        return errors.New("bad email") // logged server-side only, never echoed
    }
    return nil
}

gbl.Post("/signup", func(c *gbl.Context, s Signup) string {
    rid := gbl.MustValue[string](c, "requestID") // value set by middleware
    return "signed up: " + s.Email + " [" + rid + "]"
})

gbl.Get("/admin", func(c *gbl.Context) {
    if !isAdmin(c) {
        gbl.Abort(http.StatusForbidden, "forbidden") // an explicit abort IS echoed
    }
})
```

---

## 3. Extension points

gbl turns every policy into a swappable extension; the framework itself only
orchestrates and never hard-binds an implementation.

### 3.1 Independent Engine instances
The package-level `gbl.Get/Run/...` are a facade over the default engine. For
**isolated** engines (e.g. one per test) use `New()`:
```go
e := gbl.New()
e.Group("").Get("/x", func() string { return "y" })
e.Use(myMiddleware)
e.UseCors(corsCfg)
e.ServeHTTP(rec, req) // implements http.Handler — drive it with httptest, no port needed
```

### 3.2 Context (the shared middleware / injection handle)
`*Context` is both the middleware argument and an injectable handler parameter.
Common API:

| Method | Purpose |
|---|---|
| `c.Next()` | run the next link in the chain |
| `c.Set(k, v)` / `c.Get(k)` / `gbl.Value[T](c,k)` / `gbl.MustValue[T](c,k)` | request-scoped values (typed retrieval) |
| `c.Param(name)` / `c.Query(name)` | read a path param / query value |
| `c.JSON(status, v)` / `c.String(status, body)` / `c.Status(code)` | write the response directly |
| `c.Result()` / `c.IsWritten()` | read / detect the handler result after the chain, to override |
| `c.StatusCode()` / `c.Size()` | effective status code and body size (for metrics/tracing middleware) |
| `c.Abort()` / `c.AbortWithStatus(s)` / `c.AbortWithStatusJSON(s, v)` | stop the chain |

### 3.3 Pluggable Logger
The default writes `gbl: LEVEL msg k=v` through the standard `log` package. To
use another backend, implement the three-method interface and install it with
`SetLogger`:
```go
type Logger interface {
    Info(msg string, keysAndValues ...any)
    Warn(msg string, keysAndValues ...any)
    Error(msg string, keysAndValues ...any)
}
```
- Only want a different target/prefix? No implementation needed:
  `gbl.SetLogger(gbl.NewStdLogger(yourLogger))`.
- **Optionally implement `BannerLogger` (`Banner(block string)`)**: the startup
  banner is then emitted verbatim, without a timestamp/level prefix crowding the
  ASCII art.
- **Per-level file logging is configuration-driven** (see section 4). When a
  custom Logger is installed, the built-in file routing steps aside with a
  startup notice instead of overriding your sink.

### 3.4 Validation convention `Validatable`
Any bound business parameter (including a pointer) that implements
`Validate() error` is validated automatically; types that don't pay nothing:
```go
type Validatable interface { Validate() error }
```

You write **one `Validate()` per type**, not one per field — every field's
check lives inside that single method. gbl stays zero-dependency, so there is
no tag-based rules engine; with many fields a table-driven method keeps it
flat, and you can still call your own validator library from inside
`Validate()`. A non-nil error stops the request with a generic 400; the error
text is written to the server log only, never echoed to the client.

```go
type Signup struct {
    Email string
    Age   int
    Name  string
}

// One method covers all fields: adding a field means adding a row, not copy-paste.
func (s Signup) Validate() error {
    checks := []struct {
        field string
        err   error
    }{
        {"email", errIf(s.Email == "", "required")},
        {"age",   errIf(s.Age < 18, "must be 18+")},
        {"name",  errIf(len(s.Name) > 32, "too long")},
    }
    for _, c := range checks {
        if c.err != nil {
            return fmt.Errorf("field %q: %w", c.field, c.err) // server log only; client sees 400
        }
    }
    return nil
}

func errIf(bad bool, msg string) error {
    if bad {
        return errors.New(msg)
    }
    return nil
}
```

### 3.5 Panic renderer `PanicProcessor`
Once registered it owns rendering of every recovered value (both explicit
aborts and genuine crashes) — you take responsibility for not leaking
internal errors:
```go
gbl.RegisterPanicProcessor(func(err any) (any, gbl.HttpStatus) {
    return map[string]any{"error": "server error"}, 500
})
// Or per-engine: e.SetPanicProcessor(fn)
```

### 3.6 Explicit abort `Abort`
```go
gbl.Abort(status int, msg string) // called from a handler/middleware; status+msg reach the client
```
Unlike a bare `panic`: only `Abort` and the framework's own aborts are echoed;
every other crash becomes a generic `500` whose detail is logged only.

### 3.7 Observability middleware (built-in & custom)
`gbl.AccessLog` is a ready-made middleware (method/path/status/size/duration).
Metrics, tracing and the like are middleware too: read `StatusCode()/Size()`
around `c.Next()` the same way — the framework ships no transport dependency.

### 3.8 CORS
Install with `SetCorsConfig(*CorsConfig)` (default engine) or
`UseCors(*CorsConfig)` (instance); `Run` then wraps the chain with CORS handling:
preflight, origin/method/header matching, credentials and `max-age`.

> Note: `CorsConfig`'s fields are currently unexported and its public
> construction path is still being finalized; watch upcoming versions for a
> builder if you need to configure CORS from application code.

---

## 4. Configuration (application.ini)

At startup the framework reads the `[gbl]` section of `application.ini` in the
working directory; **omitting it means all defaults**. Every key accepts both
`camelCase` and `kebab-case`.

```ini
[gbl]
name       = myapp
port       = 9801
certFile   = server.crt          ; with keyFile, TLS is enabled
keyFile    = server.key
readTimeout       = 10s
readHeaderTimeout = 5s
writeTimeout      = 10s
idleTimeout       = 60s

; File logging (blank = file logging off, stderr only)
logDir  = ./logs                 ; auto-creates info.log/warn.log/error.log in the dir
logFile = ./app.log              ; every level merged into one file (lowest priority)
; Or set each level explicitly; precedence: per-level path > logDir/<level>.log > logFile
; infoLog  = ./logs/info.log
; warnLog  = ./logs/warn.log
; errorLog = ./logs/error.log
```

| Key | Default | Meaning |
|---|---|---|
| `name` | `gbl` | name |
| `port` | `9801` | listen port |
| `certFile` / `keyFile` | empty | both set ⇒ HTTPS enabled |
| `readTimeout` / `writeTimeout` / `readHeaderTimeout` / `idleTimeout` | 10s / 10s / 5s / 60s | server timeouts |
| `logFile` | empty | combined log file (coarsest destination) |
| `logDir` | empty | directory holding auto-named `info.log/warn.log/error.log` |
| `infoLog` / `warnLog` / `errorLog` | empty | explicit per-level path, highest priority |

---

## 5. Install & run

```bash
go get github.com/gblgo/gbl
```

```go
package main

import "github.com/gblgo/gbl"

func main() {
    gbl.Get("/", func() string { return "Hello, gbl!" })
    gbl.Run() // http://localhost:9801
}
```

```bash
go run .
```

---

## License
[MIT](./LICENSE) © lutme
