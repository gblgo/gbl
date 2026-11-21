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
  core feature is — <b>write a handler with any signature and the framework
  binds every argument and serializes the result for you.</b>
</p>

---

## Contents

This page is a **complete reference to gbl's public API**: every exported
function, type, interface and method, each with a runnable example.

1. [Getting started](#1-getting-started)
2. [Handler signatures & binding](#2-handler-signatures--binding)
3. [Routing: package-level verbs](#3-routing-package-level-verbs)
4. [Groups (`Group` / `RouteGroup`)](#4-groups-group--routegroup)
5. [Middleware (`Use` / `HandlerFunc`)](#5-middleware-use--handlerfunc)
6. [Serving (`Run` / `Shutdown`)](#6-serving-run--shutdown)
7. [`Context` reference](#7-context-reference)
8. [Request-scoped values (`Get` / `Value` / `MustValue`)](#8-request-scoped-values-get--value--mustvalue)
9. [Aborts & panic (`PanicProcessor` / `HttpStatus`)](#9-aborts--panic-panicprocessor--httpstatus)
10. [Validation (`Validatable`)](#10-validation-validatable)
11. [Logging (`Logger` / `BannerLogger` / `SetLogger` / `Info` / `Warn` / `Error`)](#11-logging-logger--bannerlogger--setlogger--info--warn--error)
12. [Upload limits (`ByteSize` / `SetMaxBodySize` / `SetMaxFileSize` / `SetMaxUploadFileSize`)](#12-upload-limits-bytesize--setmaxbodysize--setmaxfilesize--setmaxuploadfilesize)
13. [CORS (`SetCorsConfig` / `CorsConfig`)](#13-cors-setcorsconfig--corsconfig)
14. [Configuration (`Config` / `Configure`)](#14-configuration-config--configure)
15. [Startup failures (fail-fast)](#15-startup-failures-fail-fast)
16. [Response compression (`Compress`)](#16-response-compression-compress)

> **One engine, many entry points.** gbl exposes a *single* package-level engine.
> `Get/Post/Use/Group/Run/...` all act on it, so what you register and what you
> serve are guaranteed to be the same instance. Route *instances* only exist as
> `RouteGroup` handles returned by `Group`.

---

## 1. Getting started

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

    // Listens on :9801 by default; Ctrl+C / SIGTERM triggers graceful shutdown.
    gbl.Run()
}
```

```bash
go run .   # GET http://localhost:9801/hello -> hello, gbl
```

---

## 2. Handler signatures & binding

A handler is any `func`. gbl inspects its signature and binds every parameter by
**type and name**, then serializes whatever you return. Nothing else in the
framework changes how you write business logic.

### Injectable parameter types

| Parameter type | What gbl passes |
|---|---|
| `*gbl.Context` | the per-request context (also the middleware argument) |
| `*http.Request` | the inbound request |
| `http.ResponseWriter` | the raw sink, to own the response entirely |
| `*multipart.FileHeader` / `[]*multipart.FileHeader` (and value forms) | uploaded file(s) matched by field name |
| any other type | a **business value** bound by name (see below) |

### Business values bind by name

For every business parameter, **a matching path parameter always wins first**. When
there is none, the source is chosen by `Content-Type` — these two are mutually
exclusive, not a cascade:

- **JSON** (`application/json` with a non-empty body): the value comes from the
  JSON body; form and query are never consulted.
- **Form/query** (any other content type): the value comes from form/query values.
  On `POST`/`PUT`/`PATCH`, a body field and a query parameter of the same name both
  bind, with the **body winning**.

gbl's own name lookups are **exact**: a path/form/query key, or a top-level JSON
member, is matched verbatim with no case folding. (A JSON body's struct fields
are decoded by `encoding/json`, which matches field names case-insensitively.)

**json tags are optional**: a field tagged `json:"user_name"` binds by that tag
name verbatim in form/query too; `json:"-"` skips the field entirely. Without a
tag the framework derives the bind name by lowercasing the field's first letter
(`Name` → `name`, `Id` → `id`) and caches it at registration, so a request key
like `name` binds with no tag. This is the camelCase convention: name a field
`Id`, not `ID` (`ID` would derive `iD`).

```go
type User struct {
    Id   int
    Name string
}

// The path segment :id binds to the parameter named id.
gbl.Get("/user/:id", func(id string) string {
    return "user " + id
})

// POST + application/json: the whole body decodes into User.
gbl.Post("/user", func(u User) *User {
    u.Name = strings.ToUpper(u.Name)
    return &u // a pointer/struct serializes back to JSON automatically
})

// Query values bind by name too. When a name matches both a path segment
// and a query key, the path segment wins:
//   GET /user/42?verbose=true  ->  id="42" (path), verbose=true (query)
gbl.Get("/user/:id", func(id string, verbose bool) string {
    return fmt.Sprintf("user %s verbose=%v", id, verbose)
})
```

Rules that bite in practice:

**Rule 1 — a lone struct parameter takes the whole JSON body in one piece.**

```go
// POST /user  ·  Content-Type: application/json
// Body: {"id": 1, "name": "ada"}
gbl.Post("/user", func(u User) *User {
    return &u // the entire body decoded into u
})
```

**Rule 2 — with a JSON body and multiple business parameters, each non-path
parameter must appear as a top-level JSON member named like it.**

```go
// POST /user  ·  Content-Type: application/json
// Body: {"u": {"id": 1, "name": "ada"}, "verbose": true}
gbl.Post("/user", func(u User, verbose bool) string {
    return fmt.Sprintf("%s verbose=%v", u.Name, verbose)
})
```

A missing top-level member is a request-time 400, not a silent zero value —
parameter names and JSON keys must line up one-to-one.

**Struct fields must be exported.** JSON binding uses `encoding/json`, which
only sets exported (capitalized) fields. An unexported field is silently
ignored and keeps its zero value, with no error reported.

```go
type User struct {
    Name string // exported — bound from the "name" key
    age  int    // unexported — never bound, stays 0
}
```

**Rule 3 — in form/query mode a struct's fields are filled from path/form
values by bind name (the json tag name verbatim, else the field's first letter
lowercased), matched exactly, so `:id` can populate `u.Id`.**

```go
// GET /user/42  →  u.Id == 42, bound from the :id path segment
gbl.Get("/user/:id", func(u User) string {
    return "user " + strconv.Itoa(u.Id)
})
```

In **JSON** mode this cross-population does not happen — `u` comes only from
the body.

### Automatic result serialization

```go
gbl.Get("/text", func() string { return "plain" })         // text/plain
gbl.Get("/num", func() int { return 42 })                  // "42" as text
gbl.Get("/json", func() map[string]int { return map[string]int{"n": 3} }) // JSON
gbl.Get("/obj", func() *User { return &User{Id: 1} })      // JSON
gbl.Get("/none", func() { })                               // nil result -> empty body
```

### When you want the raw types

```go
gbl.Get("/raw", func(w http.ResponseWriter) {
    w.WriteHeader(http.StatusCreated)
    _, _ = w.Write([]byte("direct"))
})

gbl.Post("/echo", func(r *http.Request) string {
    return r.Header.Get("X-Trace-Id")
})
```

---

## 3. Routing: package-level verbs

One verb per registration. Each takes `path` + handler. Supported verbs and the
automatic fallbacks:

```go
gbl.Get("/items", listItems)               // GET
gbl.Post("/items", createItem)             // POST (body read per Content-Type)
gbl.Put("/items/:id", replaceItem)         // PUT (body)
gbl.Patch("/items/:id", updateItem)        // PATCH (body)
gbl.Delete("/items/:id", deleteItem)       // DELETE (no body; path/query only)
gbl.Head("/items", headItems)              // HEAD (optional; see below)
gbl.Options("/items", myOptions)           // OPTIONS (optional; see below)
```

### `Head` fallback

Registering `Head` is optional. An **unregistered** `HEAD` automatically reuses
the `GET` route for the same path.

```go
// Only GET is registered, yet HEAD /health still answers 200 (headers only).
gbl.Get("/health", func() string { return "ok" })

// Client: HEAD /health
// Response: 200 OK  Content-Type: text/plain  Content-Length: 2  (empty body,
// net/http drops the body but keeps the length and headers.)
```

### `Options` fallback

An **unregistered** `OPTIONS` is answered by the framework with `204 No Content`
and an `Allow` header listing the methods the path really supports. Registering
a handler opts that path out of the auto-answer.

```go
// Only POST is registered on this path.
gbl.Post("/items", createItem)

// Client: OPTIONS /items
// Response: 204 No Content  Allow: POST

// Once you register OPTIONS, your handler takes over the answer:
gbl.Options("/items", func(w http.ResponseWriter) {
    w.Header().Set("Allow", "POST, OPTIONS")
    w.WriteHeader(http.StatusNoContent)
})
```

### Body methods vs. non-body methods

`POST`/`PUT`/`PATCH` read and bind the request body by `Content-Type`.
`GET`/`HEAD`/`DELETE`/`OPTIONS` never read a body — any body a client sends on
these verbs is discarded, so only path and query parameters bind.

```go
// DELETE reads only the path/query.
gbl.Delete("/items/:id", func(id string, hard bool) string {
    // id comes from :id, hard comes from ?hard=true
    return fmt.Sprintf("delete %s hard=%v", id, hard)
})

// Client: DELETE /items/7?hard=true   (any body is ignored)
// Handler receives: id="7", hard=true
```

---

## 4. Groups (`Group` / `RouteGroup`)

`Group(prefix string) RouteGroup` returns a **path-prefixed, middleware-scoped**
registration handle. Its methods mirror the package-level verbs but prepend the
prefix and attach group-scoped middleware; **every method returns the group
itself**, so calls chain.

```go
api := gbl.Group("/api")

// Chaining works because each registration returns the group.
api.
    Get("/ping", func() string { return "pong" }).        // /api/ping
    Post("/ping", func() string { return "posted" })      // /api/ping

// Scoped middleware runs only for routes registered through this group.
api.Use(func(c *gbl.Context) {
    if c.Request.URL.Query().Get("token") == "" {
        c.AbortWithStatusString(http.StatusUnauthorized, "missing token")
        return
    }
    c.Next()
})
api.Get("/secret", func() string { return "top" })        // /api/secret
```

`RouteGroup` interface:

```go
type RouteGroup interface {
    GroupUse(...HandlerFunc) RouteGroup
    Get(string, any) RouteGroup
    Head(string, any) RouteGroup
    Post(string, any) RouteGroup
    Put(string, any) RouteGroup
    Patch(string, any) RouteGroup
    Delete(string, any) RouteGroup
    Options(string, any) RouteGroup
}
```

> Group middleware is scoped: `Group("/admin").GroupUse(...)` only runs for routes
> **registered through that group** — it does not leak onto sibling routes or into
> the engine-level `Use` chain.

```go
// Global middleware: every route hits this.
gbl.Use(func(c *gbl.Context) {
    log.Println("global")
    c.Next()
})

// Admin-only guard: only /admin/* routes run it.
admin := gbl.Group("/admin")
admin.GroupUse(func(c *gbl.Context) {
    if c.Request.URL.Query().Get("token") != "secret" {
        c.AbortWithStatusString(http.StatusUnauthorized, "invalid token")
        return
    }
    c.Next()
})
admin.Get("/stats", func() string { return "stats" })    // global + admin guard

// A sibling route that never went through the admin group:
gbl.Get("/public", func() string { return "public" })    // global only

// Another group doesn't pick up the admin guard either:
user := gbl.Group("/user")
user.Get("/profile", func() string { return "profile" }) // global only
```

`GET /admin/stats?token=wrong` returns 401; `GET /public` and
`GET /user/profile` still return **200**, because the admin guard isn't in
their chain at all.

---

## 5. Middleware (`Use` / `HandlerFunc`)

```go
type HandlerFunc func(c *Context)
```

`HandlerFunc` is one link in the onion chain. The chain is an ordered slice —
engine `Use` (outermost) → group `Use` → … → the matched handler (innermost) —
and `Context` keeps a cursor into it.

**`c.Next()` runs all the links after the current one** (the remaining
middleware and, finally, the handler) and then returns. Code before `c.Next()`
is therefore the **request** phase; code after it is the **response** phase,
which unwinds innermost-first. The framework calls `Next()` once to start the
chain; each middleware calls it to delegate downstream.

Both phases are code **you write inside a middleware function**, split by the
`c.Next()` call — they are not the handler and not the framework writing the
HTTP response. The handler is the innermost business function: it has no
phases and never calls `Next()`. "Response" names the outbound direction
(after the handler), not the act of writing bytes: the framework writes the
staged body to the connection only after the whole chain unwinds. Timing around
`Next()` therefore measures request-processing time (downstream + handler +
serialization) — which is why access logs sit in this phase, where the result
and status are already known.

`Next()` is an ordinary blocking call, not a scheduler: it runs every
downstream link synchronously and returns only when they have all finished.
The "response phase" is just the middleware's own code after the call, running
because the call returned — there is no second mechanism inside `Next`. For a
chain `[A, B, handler]`, execution flows straight down this diagram:

```text
request
  │
  ▼
  A.before Next()          ┐
  │                        │ request phase
  ▼                        │ (each Next() goes one layer deeper)
  B.before Next()          ┘
  │
  ▼
  handler                  ── run business, stage the result
  │
  ▼
  B.after Next()           ┐
  │                        │ response phase
  ▼                        │ (each Next() returns, unwinding outward)
  A.after Next()           ┘
  │
  ▼
write staged result
  │
  ▼
response
```

**How `Next()` works internally**: Each call to `c.Next()` increments a shared
cursor and loops through the remaining handlers. When a middleware calls
`c.Next()`, it **recursively enters** the `Next()` function again, creating a
new stack frame. All nested calls share the same cursor (`c.index`), so the
innermost call pushes it past the end, causing each outer loop to exit in turn.
This is why no handler runs twice, and why the response phase unwinds in
reverse order (innermost-first).

All nested `Next` calls share one cursor; the innermost call pushes it past
the end, so each outer loop returns in turn and no link runs twice.

**To stop the chain, call an `Abort` method** (`Abort`, `AbortWithStatusString`,
`AbortWithStatusJSON`). It pushes the cursor past the end, so every link that
has not started yet — the handler included — is skipped. The `AbortWithStatusString`
/ `AbortWithStatusJSON` variants also write the denial response in the same call.

> Returning from a middleware **without** calling `c.Next()` does **not** stop
> the chain: the enclosing `Next` loop still advances to the next link. Only
> `Abort` halts downstream.

> **`Abort` stops only downstream, not upstream.** Middleware that has already
> called `c.Next()` will still execute its "after `Next()`" code when `Next()`
> returns, even if a downstream link aborted. This is by design: access logs,
> metrics and cleanup code in the response phase must run regardless of whether
> the request succeeded or was rejected.

```go
// Example: Abort in a downstream middleware
gbl.Use(func(c *gbl.Context) {
    log.Println("mw1: before Next")
    c.Next()
    log.Println("mw1: after Next")  // ← This STILL runs even if mw2 aborts
})

gbl.Use(func(c *gbl.Context) {
    log.Println("mw2: before Abort")
    c.AbortWithStatusString(401, "unauthorized")
    log.Println("mw2: after Abort")  // ← This DOES NOT run (unreachable)
    // mw2 returns, Next() returns to mw1
})

gbl.Get("/api", func() string {
    log.Println("handler")  // ← This DOES NOT run (aborted)
    return "secret"
})

// Request GET /api logs:
//   mw1: before Next
//   mw2: before Abort
//   mw1: after Next        ← post-Next code always runs
// Response: 401 unauthorized
```

**To skip post-Next code when aborted**, check `c.Aborted()`:

```go
gbl.Use(func(c *gbl.Context) {
    c.Next()
    if !c.Aborted() {
        log.Println("success path")  // ← only runs when not aborted
    } else {
        log.Println("abort path")    // ← only runs when aborted
    }
})
```

> **Why does post-Next code always run?** `c.Next()` is an ordinary function
> call that creates a stack frame. When a downstream middleware aborts, it sets
> a flag (`index = abortIndex`) and returns, but the upstream `Next()` call is
> already on the stack and must wait for the function to return. This is Go's
> normal function call behavior — there is no way to forcibly exit a stack frame
> without `panic`, which would be inappropriate for control flow.

**This is by design, not a limitation.** Post-Next code must run regardless of
success or failure:

```go
// Access logging: every request needs a log entry, including rejected ones
gbl.Use(func(c *gbl.Context) {
    start := time.Now()
    c.Next()
    // This runs even when aborted, so you can log 401/403 rejections
    log.Printf("%s %s %d %v", 
        c.Request.Method, 
        c.Request.URL.Path,
        c.StatusCode(),  // ← 401 if aborted, 200 if success
        time.Since(start))
})

// Authentication middleware
gbl.Use(func(c *gbl.Context) {
    if !authenticated {
        c.AbortWithStatusString(401, "unauthorized")
        return
    }
    c.Next()
})

// Request: GET /api (unauthenticated)
// Flow: logger(before) → auth → Abort(401) → logger(after: logs 401)
// Output: GET /api 401 1.2ms  ← rejected requests are still logged
```

If Abort forcibly exited the entire chain, the logger's post-Next code would
never run, and you would lose all logs for rejected requests — making it
impossible to monitor authentication failure rates or audit security events.

**Flexibility: you choose what runs.** Use `c.Aborted()` to conditionally
execute post-Next logic:

```go
gbl.Use(func(c *gbl.Context) {
    c.Next()
    
    // Always run: metrics for all requests (success + failure)
    metrics.Record(c.StatusCode(), c.Size())
    
    // Only on success: cache the result
    if !c.Aborted() {
        cache.Store(key, c.Result())
    }
    
    // Only on abort: audit rejections
    if c.Aborted() {
        audit.LogRejection(c.Request, c.StatusCode())
    }
})
```

**Industry standard.** Gin, the most popular Go web framework, uses the exact
same design: Abort stops downstream but post-Next code in upstream middleware
always runs. This is the standard pattern for Go web frameworks with
middleware chains.

```go
// A middleware: its body is split into two phases by c.Next().
gbl.Use(func(c *gbl.Context) {
    // ── request phase: before going downstream ──
    start := time.Now()
    c.Set("requestID", newRequestID())

    c.Next() // runs all downstream middleware + the handler, then returns

    // ── response phase: the handler has already finished ──
    log.Printf("took %s, status %d", time.Since(start), c.StatusCode())
})

// The handler is plain business logic: no phases, no Next().
gbl.Get("/ping", func() string { return "pong" })

// gbl.Use is variadic — install several links at once:
gbl.Use(recoverer, requestID)
```

The middleware chain, in order: engine-level `Use` (outermost) → group `Use` →
… → the matched handler (innermost). The "after `c.Next()`" halves unwind
innermost-first.

```go
// Demonstrates the unwind order.
gbl.Use(func(c *gbl.Context) {
    fmt.Println("engine before")
    c.Next()
    fmt.Println("engine after")
})

api := gbl.Group("/api")
api.Use(func(c *gbl.Context) {
    fmt.Println("group before")
    c.Next()
    fmt.Println("group after")
})
api.Get("/ping", func() string {
    fmt.Println("handler")
    return "pong"
})

// Request GET /api/ping logs, in this exact order:
//   engine before
//   group before
//   handler
//   group after
//   engine after
```

### Request lifecycle, end to end

What happens from the moment a request arrives until the response reaches the
client:

1. **Accept & dispatch.** `net/http` accepts the connection, parses the request
   and calls the server handler — the engine itself, or a `corsHandler` wrapping
   it when `SetCorsConfig` was called.
2. **CORS (only if configured).** `corsHandler` checks `Origin`: a preflight is
   answered directly, an allowed origin gets the CORS headers, then it delegates
   to the engine. No `Origin` goes straight to the engine.
3. **Method & route match.** The engine resolves the verb (unknown → 501) and
   walks that method's route tree, filling path parameters. An unregistered
   `HEAD` borrows the `GET` route. No match → 404, or 405 + `Allow` (204 for
   `OPTIONS`).
4. **Build the Context.** A pooled `Context` is primed with the request, the
   matched path params, the route's frozen chain and the logger. The chain is
   `[global middleware…, group middleware…, endpoint]`.
5. **Run the chain — `c.Next()`.** The chain is a slice of `HandlerFunc`; its
   last link is not the business handler itself but `routeEndpoint`, an ordinary
   middleware whose whole body is `c.invokeHandler()`:

```go
// the last link of every chain (context.go)
var routeEndpoint HandlerFunc = func(c *Context) { c.invokeHandler() }

// assembled at startup (route.go): global mw + group mw + routeEndpoint
chain = append(chain, e.middlewares...)
chain = append(chain, meta.mw...)
chain = append(chain, routeEndpoint)
```

   This chain is built **once, at startup**, by `generateRoute`: registering a
   route (`Get`/`Post`/…) only queues it; `Run()` then assembles the chain and
   freezes it into the route's `routeHandler.handlers`, with `routeEndpoint` as
   the last link. Every request reuses that frozen slice (`newContext` sets
   `c.handlers = rh.handlers`) — nothing is re-assembled per request.

   `Next()` just loops over this slice calling each link. When it reaches
   `routeEndpoint`, that call runs `invokeHandler()`, which parses, binds,
   validates, reflectively calls the business handler (stored in `c.rh`) and
   serializes the result. The onion nesting:

```text
engine.Next() → handlers[0]
└─ global middleware
   ├─ request-phase code
   ├─ c.Next() → handlers[1]
   │  └─ group middleware
   │     ├─ request-phase code
   │     ├─ c.Next() → handlers[2]
   │     │  └─ routeEndpoint
   │     │     └─ c.invokeHandler():
   │     │        parse → bind → validate → call handler → serialize c.bytes
   │     └─ response-phase code (after Next returns)
   └─ response-phase code (after Next returns)
```

6. **Write the response.** If a middleware or the handler already wrote a body
   (`c.JSON`, `c.String`, or an injected `http.ResponseWriter`), that stands.
   Otherwise the engine writes the staged `c.bytes` with the content type chosen
   at registration, flushing the recorded status (default 200) first.
7. **Cleanup.** Multipart temp files are removed; the `Context`, path-param map
   and response writer are reset and returned to their pools.
8. **Any panic** along the way is recovered and rendered: a framework-generated
   abort reaches the client verbatim, a registered `PanicProcessor` owns
   rendering, and anything else is logged and answered with a generic 500.

The handler itself (step 5, innermost) is plain business code: it neither calls
`Next()` nor has phases. The "response phase" belongs to middleware, on the way
out — which is why timing and access logs read the result there.

---

## 6. Serving (`Run` / `Shutdown`)

### `Run()`

Starts the single engine on the configured address and **blocks** until it
stops. On `SIGINT`/`SIGTERM` it performs a graceful shutdown (listener closes,
in-flight requests get a window to finish). Any fatal startup error panics.

```go
gbl.Get("/hi", func() string { return "hi" })
gbl.Run() // port from Configure, default :9801
```

> **The engine is read-only while serving.** `Run` freezes every assembling
> decision — route registration, `Use`, `SetLogger`, `Configure`, CORS, size
> limits and the panic processor. Calling any of them after `Run` (or calling
> `Run` twice) panics with an explicit *"must be called before Run"* message
> instead of being silently swallowed.

### `Shutdown(ctx context.Context) error`

Gracefully stops the server started by `Run`, waiting for active requests until
`ctx` expires. Safe to call more than once and from a different goroutine than
`Run`. Calling it before `Run` returns `http.ErrServerClosed`.

```go
// Typical: shut down from another goroutine (e.g. an admin endpoint).
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
if err := gbl.Shutdown(ctx); err != nil {
    log.Fatal(err)
}
```

> `Run` already installs a signal handler for graceful shutdown; use `Shutdown`
> when *you* need to trigger it (a health-check kill switch, tests, embedding).

---

## 7. `Context` reference

`*Context` is threaded through the middleware chain and, when a handler declares
it, injected as a parameter. It carries the request, the matched path
parameters and a key/value bag for passing data between links and to the
handler; the response is written through `c.JSON` / `c.String` / `c.Status`.

> **A `Context` is pooled and valid only within the current request.** As soon
> as the request ends, it is cleared and reused for the next one. So do not store
> a `*Context` in a global or cache, and do not capture it in a goroutine or
> callback that runs after the request finishes — you would read another
> request's data or zero values, and concurrent access is a data race. Copy out
> whatever you need asynchronously or across requests (e.g. the results of
> `c.PathParam`, `c.Get`) into your own variables before the request ends.
> The injected `http.ResponseWriter` is pooled too and follows the same rule:
> never retain it across requests.

Exported field:

```go
c.Request // *http.Request — the inbound request
```

### Methods

| Method | Purpose |
|---|---|
| `Next()` | run the remaining chain (and the handler at its end) |
| `PathParam(name) string` | a matched path parameter by name (for middleware; handlers bind path params by name) |
| `Set(key, value)` / `Get(key) (any, bool)` | request-scoped values (typed helpers: `Value` / `MustValue`, §8) |
| `Result() any` | the handler's return value as `any` (valid after `Next()`); the boxing happens once, on first read |
| `IsWritten() bool` | whether a body has already been committed |
| `StatusCode() int` | the status that will be / was sent (200 default) |
| `Size() int` | effective response body length |
| `Aborted() bool` | whether the chain has been stopped |
| `Abort()` | stop the chain, write nothing |
| `AbortWithStatusString(status int, body string)` | deny with a plain-text body |
| `AbortWithStatusJSON(status int, body any)` | deny with a JSON body |

### Examples

```go
// Scenario: many endpoints need to know "who is the logged-in user". Requests
// carry Authorization: Bearer <token>. Auth is cross-cutting — dozens of routes
// share it, and an unauthenticated call must be rejected before any handler runs
// — so it lives in ONE middleware that resolves the token to a uid.
gbl.Use(func(c *gbl.Context) {
    uid, ok := parseToken(c.Request.Header.Get("Authorization")) // your logic
    if !ok {
        c.AbortWithStatusString(http.StatusUnauthorized, "unauthorized")
        return
    }
    c.Set("uid", uid)
    c.Next()
})

// The handler never parses a token; it just uses the current user. uid comes
// from the token, not from path/query/body, so auto-binding cannot supply it —
// Context is the only channel from the middleware to the handler.
gbl.Get("/orders", func(c *gbl.Context) []Order {
    v, _ := c.Get("uid") // any
    return loadOrders(v.(int64))
})

// Scenario: per-endpoint monitoring — record every request's status, size and
// latency. After the handler ran, only Context exposes the staged result and
// the effective status/size; an injected http.ResponseWriter can write but
// cannot read any of these.
gbl.Use(func(c *gbl.Context) {
    c.Next()
    metrics.Observe(c.StatusCode(), c.Size())
    if c.Aborted() {
        log.Printf("request aborted with status %d", c.StatusCode())
    }
})
```

---

## 8. Request-scoped values (`Get` / `Value` / `MustValue`)

Middleware stores values with `c.Set`; the handler (or a later link) reads them
with `c.Get`, which returns the raw `any` — assert it to the type you stored:

```go
gbl.Use(func(c *gbl.Context) {
    c.Set("uid", int64(7))
    c.Next()
})

gbl.Get("/me", func(c *gbl.Context) string {
    v, ok := c.Get("uid")
    if !ok {
        return "anonymous"
    }
    return fmt.Sprintf("user %d", v.(int64))
})
```

You can also skip the manual assertion with the typed helpers `Value` (returns an
ok flag) and `MustValue` (zero value on miss). They are package-level functions
because Go methods cannot carry type parameters:

```go
uid, ok := gbl.Value[int64](c, "uid")           // ok == true, uid == 7
name := gbl.MustValue[string](c, "displayName") // "" when absent / wrong type
```

---

## 9. Aborts & panic (`PanicProcessor` / `HttpStatus`)

The error policy of gbl splits echo rights by **who authored the message**:
**developer-authored text reaches the client verbatim** — your `PanicProcessor`
(if installed) and the error text returned by `Validate`;
**framework-assembled internals never do** — panic causes and binding failures
(parameter names, Go type strings, strconv detail) go to the server log only,
with a generic `500` / `400` on the wire. Request-parse errors (bad Content-Type,
oversize body) describe the client's own request framing and carry no internal
identifiers, so they are echoed.

### `PanicProcessor` / `RegisterPanicProcessor` / `HttpStatus`

```go
type PanicProcessor func(err any) (any, HttpStatus)
type HttpStatus int
```

`RegisterPanicProcessor(p PanicProcessor)` installs your panic renderer on the
default engine. **As soon as one is registered it owns rendering of every
recovered value** — explicit aborts *and* genuine crashes — so you take on the
responsibility of not leaking internal errors. The returned body can be any
renderable value (`string`, `error`, `struct`, `map`, …).

```go
gbl.RegisterPanicProcessor(func(err any) (any, gbl.HttpStatus) {
    // Return a safe, uniform envelope; never format err into the body.
    return map[string]any{"code": 500, "message": "internal error"}, gbl.HttpStatus(500)
})
```

---

## 10. Validation (`Validatable`)

```go
type Validatable interface { Validate() error }
```

Any bound **business parameter** (including a pointer) whose type implements
`Validate() error` is checked automatically **after binding and before the
handler runs**. Types that don't implement it pay nothing.

```go
type Signup struct {
    Email string
    Age   int
    Name  string
}

// You write ONE Validate per type — every field's rule lives inside it.
// gbl imposes no rules of its own: use plain Go, or call your own validator.
func (s Signup) Validate() error {
    if s.Email == "" {
        return errors.New("email is required")
    }
    if s.Age < 18 {
        return errors.New("must be 18 or older")
    }
    if len(s.Name) > 32 {
        return errors.New("name is too long")
    }
    return nil
}

// The handler never nil-checks; a non-nil Validate error yields a 400 echoing its text.
gbl.Post("/signup", func(s Signup) string {
    return "signed up: " + s.Email
})
```

A non-nil error stops the request with a **400 whose body is the error text
verbatim** — you wrote the message, so you own what it discloses: write what the
client should see, and keep anything sensitive out of `Validate`. Framework-side
binding failures are the opposite: a generic `400` only, with the reflection
detail in the server log (see section 9).

---

## 11. Logging (`Logger` / `BannerLogger` / `SetLogger` / `Info` / `Warn` / `Error`)

Two sources, one exit. Every message the framework emits internally (a
recovered panic, a binding failure, a startup abort) and every business line
written through `gbl.Info` / `gbl.Warn` / `gbl.Error` flows through the single
`Logger` installed on the engine. Destination, format and coloring are then
entirely the sink's business.

### The default console (a development aid)

Until you call `SetLogger`, framework and facade logs print to the console:

```
2026-11-20 16:12:38 INFO started port=9801
```

* Shape: `LEVEL msg key=value ...` behind the standard date/time prefix.
* The level word is tinted on a real terminal — green `INFO`, yellow `WARN`,
  red `ERROR`. Piped, redirected or CI output stays plain text, and setting
  the `NO_COLOR` environment variable opts out even on a terminal.
* The default sink is a **private** `log.Logger`: the global standard-library
  logger is never touched. Your `log.SetOutput` / `log.SetFlags` calls do not
  move or restyle framework lines, and framework logging never captures or
  colors your own `log.Println` output. The two channels are fully isolated.

### `Logger`

```go
type Logger interface {
    Info(msg string, keysAndValues ...any)
    Warn(msg string, keysAndValues ...any)
    Error(msg string, keysAndValues ...any)
}
```

`msg` describes the event, then alternating key/value pairs. A structured
backend (zap/slog) consumes them directly; the default keeps a plain-text shape.
This interface is *the* integration contract: implement it, install it, done.

### `SetLogger`

```go
gbl.SetLogger(myLogger)   // nil is ignored, so a working logger always remains
```

Like every assembling API it must run **before `Run`** (the engine is read-only
while serving, §6). Integrating zap/slog/zerolog is a thin adapter that
forwards `msg` and the pairs to your logger instance — style decisions
(whether the level word appears in the text at all) belong to that library.

### `Info` / `Warn` / `Error` (business entry)

```go
gbl.Get("/user", func(id string) string {
    gbl.Info("user loaded", "id", id) // → the engine's installed Logger
    return fetch(id)
})
```

The package-level trio is the ergonomic entry for business code: once a logger
is injected with `SetLogger` it is the framework's sink, and calling through
these functions needs no reference to keep or fetch. Framework-internal events
use the same exit, so one install decides where *all* logs land.

### `BannerLogger` (optional)

```go
type BannerLogger interface { Banner(block string) }
```

A `Logger` that *also* implements `BannerLogger` is handed the startup banner as
a pre-rendered multi-line block and may emit it verbatim, without a
timestamp/level prefix crowding the ASCII art. `Logger` alone still receives the
banner, just as an ordinary `Info` record.

```go
type appLogger struct{ ... }

func (l appLogger) Info(msg string, kv ...any) { ... }
func (l appLogger) Warn(msg string, kv ...any) { ... }
func (l appLogger) Error(msg string, kv ...any) { ... }

func (l appLogger) Banner(block string) {   // optional companion
    fmt.Println(block)                       // print the art clean, no prefix
}

gbl.SetLogger(appLogger{})
```

### Request monitoring is yours

The framework ships no access-log middleware on purpose. Watching how APIs are
called is an application concern, and the chain already gives you everything
one line of middleware needs — install your own:

```go
gbl.Use(func(c *Context) {
    start := time.Now()
    c.Next()
    gbl.Info("access", "path", c.Request.URL.Path,
        "status", c.StatusCode(), "size", c.Size(), "cost", time.Since(start))
})
```

### File logging (integration example)

The framework itself prints **only to the console**. To write files, implement
the interface against any writer — for instance the standard `log` package
pointed at a file:

```go
type fileLogger struct{ l *log.Logger }

func (s fileLogger) Info(msg string, kv ...any)  { s.emit("INFO", msg, kv) }
func (s fileLogger) Warn(msg string, kv ...any)  { s.emit("WARN", msg, kv) }
func (s fileLogger) Error(msg string, kv ...any) { s.emit("ERROR", msg, kv) }

func (s fileLogger) emit(level, msg string, kv []any) {
    s.l.Printf("%s %s %v", level, msg, kv)
}

f, err := os.OpenFile("app.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
if err != nil {
    log.Fatal(err)
}
gbl.SetLogger(fileLogger{l: log.New(f, "", log.LstdFlags)})
```

Rotation, per-level files, console+file fan-out and richer routing belong to a
zap/slog adapter — or the future gbl-boot integration layer; the kernel ships no
built-in file logging.

---

## 12. Upload limits (`ByteSize` / `SetMaxBodySize` / `SetMaxFileSize` / `SetMaxUploadFileSize`)

```go
type ByteSize int
```

`ByteSize` is a named `int` for a byte count — the type name itself documents
what the argument means at the call site. The rest of this section is about
**uploaded files**: they bind only on body methods carrying
`multipart/form-data`, and a file parameter is matched to the file part whose
field name equals the parameter name (matched exactly). This file
lookup is a separate path from text form/query values — the two never share a
source. See the table below for the accepted parameter types.

```go
import "mime/multipart"

// One file uploaded under the form field "avatar".
gbl.Post("/upload", func(avatar *multipart.FileHeader) string {
    return fmt.Sprintf("%s (%d bytes)", avatar.Filename, avatar.Size)
})

// Several files under the same field "photos".
gbl.Post("/gallery", func(photos []*multipart.FileHeader) string {
    return fmt.Sprintf("%d files uploaded", len(photos))
})
```

| Parameter type | Binds |
|---|---|
| `*multipart.FileHeader` / `multipart.FileHeader` | the first file under the matching field |
| `[]*multipart.FileHeader` / `[]multipart.FileHeader` | every file under the matching field |

A missing upload leaves the pointer `nil` and the slice empty — no panic.

### `SetMaxBodySize` / `SetMaxFileSize` / `SetMaxUploadFileSize`

```go
func SetMaxBodySize(size ByteSize)          // hard cap on total request body (413 over the limit)
func SetMaxFileSize(size ByteSize)          // multipart per-file in-memory threshold; larger spill to temp files
func SetMaxUploadFileSize(size ByteSize)    // hard cap on each individual uploaded file (413 over the limit)
```

```go
gbl.SetMaxBodySize(gbl.ByteSize(32 << 20))       // 32 MiB total body cap
gbl.SetMaxFileSize(gbl.ByteSize(4 << 20))        // files > 4 MiB spill to temp files
gbl.SetMaxUploadFileSize(gbl.ByteSize(5 << 20))  // allow a single file up to 5 MiB (default 1 MiB)
```

The three limits are **independent** knobs, each answering a different
question. All take a positive `ByteSize` and panic on a non-positive one; the
**Default** column is the value applied at startup until you override it.

| Setting | What it controls | Over the limit | Default |
|---|---|---|---|
| `SetMaxBodySize` | the **total size of the whole request body** (multipart uploads included) | rejected with `413` | **5 MiB** |
| `SetMaxFileSize` | the **in-memory threshold** for multipart parsing — parts up to this size stay in RAM, larger ones spill to temp files | nothing; it is *not* a cap and never rejects | **2 MiB** |
| `SetMaxUploadFileSize` | the **maximum size of each individual uploaded file** (any field, any part) | rejected with `413` before the handler runs | **1 MiB** |

Read the three together:

- `SetMaxBodySize` is the only limit on the **whole** request — a large body is
  refused regardless of how it is split across fields.
- `SetMaxFileSize` is purely a **performance** setting (memory vs. disk); it
  does **not** restrict how much you may upload.
- `SetMaxUploadFileSize` is the only **per-file** guard: it rejects one oversized
  file even when the total would still fit under `SetMaxBodySize` (checked
  against each `FileHeader.Size` after the form is parsed). Its 1 MiB default
  sits **below** the 5 MiB body default, so it bites out of the box: no single
  upload may exceed 1 MiB, though the body can still hold up to 5 MiB spread
  across smaller files. Large uploads are risky for the server, so the cap is
  enforced by default — raise it only when you genuinely need bigger files.

---

## 13. CORS (`SetCorsConfig` / `CorsConfig`)

```go
func SetCorsConfig(cc *CorsConfig)   // nil disables CORS
```

`SetCorsConfig` installs the cross-origin policy on the default engine; `Run`
then wraps the handler chain with it — preflight, origin/method/header matching,
credentials and `max-age`. `CorsConfig` is built with an ordinary struct
literal; every field is exported:

```go
type CorsConfig struct {
	AllowedOrigins   []string      // origins accepted via Access-Control-Allow-Origin, or "*"
	AllowedMethods   []string      // methods permitted for cross-origin requests
	AllowedHeaders   []string      // request headers accepted in a preflight
	ExposedHeaders   []string      // response headers readable by the cross-origin client
	AllowCredentials bool          // whether cookies/auth may accompany cross-origin calls
	MaxAge           time.Duration // how long a client may cache the preflight result
}
```

```go
gbl.SetCorsConfig(&gbl.CorsConfig{
	AllowedOrigins:   []string{"https://app.example.com"},
	AllowedMethods:   []string{http.MethodGet, http.MethodPost},
	AllowedHeaders:   []string{"Content-Type", "X-Trace-Id"},
	ExposedHeaders:   []string{"X-Total-Count"},
	AllowCredentials: true,
	MaxAge:           time.Hour,
})

// Any site may read the API — without credentials:
gbl.SetCorsConfig(&gbl.CorsConfig{
	AllowedOrigins: []string{"*"},
	AllowedMethods: []string{http.MethodGet},
})

gbl.SetCorsConfig(nil) // disable CORS again
```

A **security policy** blocks unsafe combinations — for example
`AllowCredentials=true` together with a wildcard `"*"` in `AllowedOrigins`,
which would let any site send credentialed (cookie/auth) cross-origin
requests. Such a configuration aborts startup before `Run`.

---

## 14. Configuration (`Config` / `Configure`)

The kernel **reads no configuration file**: defaults work out of the box, and
overrides live in code. Configuration files (YAML, environment, profiles,
config centers) are an integration-layer concern — the future gbl-boot will
resolve those sources and call the same `Configure` entry point.

```go
type Config struct {
    Name              string        // application name
    Port              string        // listen port, without the leading colon
    CertFile          string        // TLS certificate file path
    KeyFile           string        // TLS private key file path
    ReadTimeout       time.Duration // max duration to read an entire request
    ReadHeaderTimeout time.Duration // max duration to read request headers
    WriteTimeout      time.Duration // max duration to write an entire response
    IdleTimeout       time.Duration // max idle wait before a keep-alive connection closes
}
```

### `Configure`

```go
func Configure(fn func(*Config))
```

`fn` receives the configuration **already filled with defaults**, so it only
assigns the fields it cares about. Call it before `Run`; passing nil is a
no-op.

```go
gbl.Configure(func(c *gbl.Config) {
    c.Name = "myapp"
    c.Port = "8080"
    c.ReadTimeout = 5 * time.Second
})
```

| Field | Default | Meaning |
|---|---|---|
| `Name` | `gbl` | application name |
| `Port` | `9801` | listen port |
| `CertFile` / `KeyFile` | empty | both set ⇒ HTTPS enabled |
| `ReadTimeout` / `ReadHeaderTimeout` / `WriteTimeout` / `IdleTimeout` | 10s / 5s / 10s / 60s | server timeouts |

**TLS**: HTTPS turns on when both `CertFile` and `KeyFile` are non-empty,
re-derived after every `Configure`. Paths reach the standard library verbatim:
**a relative path resolves against the process working directory at launch**,
not the executable's directory. If the launch working directory is out of your
control, build an absolute path inside `Configure`.

> Your own business configuration (database connections, ...) is the
> application's business — the kernel only manages server runtime parameters.
> For file/env configuration, parse it in your application and call
> `Configure`, or wait for gbl-boot.

---

## 15. Startup failures (fail-fast)

Every startup and registration failure is **fail-fast**: gbl first records the
reason through the engine `Logger`, then panics — so a deployment that dies
before serving still leaves the cause in your log sink, not just a bare stack
trace. A busy port, a route conflict and a dangerous CORS policy all take this
path.

Logs go to the console by default, or to your sink when a custom `Logger` is
installed. The kernel writes no fallback files.

---

## 16. Response compression (`Compress`)

`Compress(level int) HandlerFunc` returns a middleware that gzip-compresses the response body when beneficial. Compression happens after `c.Next()` returns, so the handler has already produced its result.

**Three conditions must all hold:**

1. The client sent `Accept-Encoding: gzip`
2. The response `Content-Type` is compressible (text, JSON, XML, CSS, JS, SVG, etc.)
3. The response body is at least **1 KB** — smaller bodies gain little and the CPU cost is not worth it

When all conditions hold, the middleware replaces the staged body with compressed data and sets `Content-Encoding: gzip` and `Vary: Accept-Encoding`.

**Compression levels** (mirroring `compress/gzip`, re-exported so you need not import it directly):

| Constant | Value | Meaning |
|----------|-------|---------|
| `NoCompression` | 0 | No compression |
| `BestSpeed` | 1 | Fastest |
| `DefaultCompression` | -1 | Balanced (recommended default) |
| `BestCompression` | 9 | Smallest output, highest CPU |
| `HuffmanOnly` | -2 | Huffman encoding only |

**Global usage:**

```go
package main

import "github.com/gblgo/gbl"

func main() {
    gbl.Use(gbl.Compress(gbl.DefaultCompression))
    gbl.Get("/api/data", handler)
    gbl.Run(":8080")
}
```

**Group-scoped usage:**

```go
api := gbl.Group("/api")
api.GroupUse(gbl.Compress(gbl.BestSpeed))
api.Get("/data", handler)
```

**Middleware ordering:** Compress runs post-`Next()`, so it observes the final response. Place it **after** auth/CORS middleware (so errors skip compression) and **before** logging (so access logs see the compressed size if you want to report it). A typical order:

```go
gbl.Use(
    logger,            // outermost: sees everything
    corsMiddleware,    // handles preflight
    authMiddleware,    // denies unauthenticated
    gbl.Compress(gbl.DefaultCompression), // compresses successful responses
)
```

**Binary formats are never compressed:** images, video, audio, PDF and other binary content types are skipped automatically, since they are already compressed or gain nothing from gzip.

---

## Performance note

The hot path is already optimized with a precomputed bind plan, pooled objects
(response writer / path params / `Context` / `Context.keys`) and allocation-free route matching.
Static routes (no path parameters) use O(1) map lookup instead of the radix tree.
The remaining gap versus mature frameworks comes from the inherent cost of
reflectively invoking arbitrary handler signatures — a trade-off of this
framework's core feature, not something easily erased.

---

## License

[MIT](./LICENSE) © lutme
