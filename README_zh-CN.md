# Grand Blue Lore

<p align="center">
  <a href="README.md">English</a> · <a href="README_zh-CN.md">简体中文</a>
</p>

<p align="center">
  <b>gbl</b> 是一个 <b>零依赖</b> 的 Go Web 框架：用 Go 标准库即可跑起一个完整的 HTTP 服务，
  无需任何第三方包。它的核心特点是——<b>handler 签名随便写，框架自动帮你把参数装配好、把返回值序列化好</b>。
</p>

---

## 目录

本文是 gbl **对外 API 的完整参考**：每一个导出的函数、类型、接口和方法都会介绍并配示例。

1. [起步](#1-起步)
2. [handler 签名与参数装配](#2-handler-签名与参数装配)
3. [路由：包级动词注册](#3-路由包级动词注册)
4. [分组（`Group` / `RouteGroup`）](#4-分组group--routegroup)
5. [中间件（`Use` / `HandlerFunc`）](#5-中间件use--handlerfunc)
6. [服务启停（`Run` / `Shutdown`）](#6-服务启停run--shutdown)
7. [`Context` 详解](#7-context-详解)
8. [请求级取值（`Get` / `Value`）](#8-请求级取值get--value)
9. [中止与 panic（`PanicProcessor` / `HttpStatus`）](#9-中止与-panicpanicprocessor--httpstatus)
10. [校验（`Validatable`）](#10-校验validatable)
11. [日志（`Logger` / `BannerLogger` / `SetLogger` / `Info` / `Warn` / `Error`）](#11-日志logger--bannerlogger--setlogger--info--warn--error)
12. [上传大小限制（`ByteSize` / `SetMaxBodySize` / `SetMaxFileSize` / `SetMaxUploadFileSize`）](#12-上传大小限制bytesize--setmaxbodysize--setmaxfilesize--setmaxuploadfilesize)
13. [跨域（`SetCorsConfig` / `CorsConfig`）](#13-跨域setcorsconfig--corsconfig)
14. [配置（`Config` / `Configure`）](#14-配置config--configure)
15. [启动失败（fail-fast）](#15-启动失败fail-fast)
16. [响应体压缩（`Compress`）](#16-响应体压缩compress)

> **一个引擎，多个入口。** gbl 只暴露*一个*包级引擎。`Get/Post/Use/Group/Run/...`
> 全部作用于它，所以你注册的规则和你启动的服务必然是同一实例，不存在"注册进第二个引擎"的坑。
> 对外不存在引擎实例，路由分组只以 `Group` 返回的 `RouteGroup` 句柄存在。

---

## 1. 起步

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

    // 默认监听 :9801；Ctrl+C / SIGTERM 触发优雅停机
    gbl.Run()
}
```

```bash
go run .   # GET http://localhost:9801/hello -> hello, gbl
```

---

## 2. handler 签名与参数装配

handler 就是任意 `func`。gbl 读取它的签名，按**类型 + 名字**装配每个参数，再把你的返回值序列化出去。写业务逻辑的手感完全不变。

### 可注入的参数类型

| 参数类型 | gbl 注入什么 |
|---|---|
| `*gbl.Context` | 请求上下文（也是中间件的入参） |
| `*http.Request` | 原始请求 |
| `http.ResponseWriter` | 原始响应写入器，完全接管响应 |
| `*multipart.FileHeader` / `[]*multipart.FileHeader`（含值类型） | 按字段名匹配到的上传文件 |
| 其他任意类型 | 按名字装配的**业务值**（见下） |

### 业务值按名字装配

对每个业务参数，**先查同名路径参数，命中就直接用**。没命中时，取值来源由 `Content-Type` **二选一**（不是级联回退）：

- **JSON**（`application/json` 且 body 非空）：只从 JSON body 取，form/query 一律不看。
- **form/query**（其他 content type）：从 form/query 值取。`POST`/`PUT`/`PATCH` 下，body 字段与同名 query 都能绑定，**body 优先**。

gbl 自己的名字查找一律**精确匹配**：路径/form/query 的 key、或顶层 JSON 成员，都按原样匹配，不做大小写折叠。（JSON body 内部的结构体字段由 `encoding/json` 解码，它按字段名忽略大小写匹配。）

**json tag 可选**：结构体字段写了 `json:"user_name"` 时，form/query 绑定也按 tag 名原样匹配；`json:"-"` 的字段跳过绑定。不写 tag 时，框架把字段名首字母转小写得到绑定名（`Name` → `name`、`Id` → `id`）并在注册期缓存，于是 `name` 这样的请求 key 无需 tag 即可绑定。这就是驼峰约定：字段应命名为 `Id` 而非 `ID`（`ID` 会得到 `iD`）。

```go
type User struct {
    Id   int
    Name string
}

// 路径段 :id 装配给名为 id 的参数。
gbl.Get("/user/:id", func(id string) string {
    return "user " + id
})

// POST + application/json：整个 body 解码进 User。
gbl.Post("/user", func(u User) *User {
    u.Name = strings.ToUpper(u.Name)
    return &u // 指针/结构体自动序列化回 JSON
})

// query 值也按名字装配。若某参数名同时对应 :路径段 和 query key，路径赢：
//   GET /user/42?verbose=true  ->  id="42"（路径），verbose=true（query）
gbl.Get("/user/:id", func(id string, verbose bool) string {
    return fmt.Sprintf("user %s verbose=%v", id, verbose)
})
```

实际会踩到的规则：

**规则 1 —— 单个结构体参数：整块 JSON body 一次性解入。**

```go
// POST /user · Content-Type: application/json
// Body: {"id": 1, "name": "ada"}
gbl.Post("/user", func(u User) *User {
    return &u // 整个 body 解入 u
})
```

**规则 2 —— JSON body + 多个业务参数时，每个非路径参数都必须是一个与其形参同名的顶层 JSON 成员。**

```go
// POST /user · Content-Type: application/json
// Body: {"u": {"id": 1, "name": "ada"}, "verbose": true}
gbl.Post("/user", func(u User, verbose bool) string {
    return fmt.Sprintf("%s verbose=%v", u.Name, verbose)
})
```

缺一个顶层成员就是请求期 400，而不是静悄悄给零值 —— 形参名与 JSON key 必须一一对应。

**结构体字段必须导出。** JSON 绑定走 `encoding/json`，只会给导出（大写开头）字段赋值。未导出字段被静默忽略、保持零值，且不报错。

```go
type User struct {
    Name string // 导出 —— 从 "name" 键绑定
    age  int    // 未导出 —— 永远绑不上，恒为 0
}
```

**规则 3 —— form/query 模式下结构体字段按绑定名（json tag 名原样，无 tag 则字段名首字母转小写）精确匹配，从路径/form 值填充，故 `:id` 也能填 `u.Id`。**

```go
// GET /user/42  →  u.Id == 42，来自 :id 路径段
gbl.Get("/user/:id", func(u User) string {
    return "user " + strconv.Itoa(u.Id)
})
```

**JSON** 模式下不会这样跨填 —— `u` 只能来自 body。

### 返回值自动序列化

```go
gbl.Get("/text", func() string { return "plain" })         // text/plain
gbl.Get("/num", func() int { return 42 })                  // 文本 "42"
gbl.Get("/json", func() map[string]int { return map[string]int{"n": 3} }) // JSON
gbl.Get("/obj", func() *User { return &User{Id: 1} })      // JSON
gbl.Get("/none", func() { })                               // nil 结果 -> 空 body
```

### 想要原始类型时

```go
gbl.Get("/raw", func(w http.ResponseWriter) {
    w.WriteHeader(http.StatusCreated)
    _, _ = w.Write([]byte("direct"))
})

gbl.Post("/echo", func(r *http.Request) string {
    return r.Header.Get("X-Trace-Id")
})
```

### 生产构建：参数名与 DWARF

按名字装配需要 handler 的**参数名**，而 `reflect` 并不暴露它们。gbl 依次从两处恢复参数名：先是 handler 自己的源文件（磁盘上）——这是开发期路径（`go run`，或在源码树旁执行的普通 `go build`）；然后是二进制自带的 **DWARF** 调试信息——这是无源码的生产路径。

编译器默认就会输出 DWARF，而 `-trimpath` 只是重写记录的源码路径、并不会剥除 DWARF，所以**普通的生产构建直接就能用**：

```bash
# 没有包装命令、不用安装、没有代码生成、没有额外的构建步骤。
go build -trimpath -o app ./cmd/api
```

把这个二进制塞进容器、脱离源码运行，gbl 会从二进制自身把参数名读回来，于是 `func(id string)` 照样按名字装配。你的项目、仓库和最终镜像里**不含任何生成文件**，也**不需要任何额外工具**。

**唯一需要注意的：`-ldflags "-w"` / `"-s"`。** 这两个链接器 flag 会剥除调试信息——`-w` 禁用 DWARF，`-s` 省略符号表和调试信息。用它们构建的二进制里不再有 gbl 可读的参数名，于是按名字装配标量的 handler 会在启动时直接中止，而不是悄无声息地什么都装不上；报错信息会明确解释这一点。解决办法是改用 `struct` / `*struct` 参数来装配：结构体的字段名来自反射而非 DWARF，无论用什么构建 flag 都会保留。如果你没有剥除调试信息，那就什么都不用做。

---

## 3. 路由：包级动词注册

一次注册一个动词，参数为 `path` + handler。支持的动词及其自动兜底：

```go
gbl.Get("/items", listItems)               // GET
gbl.Post("/items", createItem)             // POST（按 Content-Type 读 body）
gbl.Put("/items/:id", replaceItem)         // PUT（读 body）
gbl.Patch("/items/:id", updateItem)        // PATCH（读 body）
gbl.Delete("/items/:id", deleteItem)       // DELETE（不读 body，仅路径/query）
gbl.Head("/items", headItems)              // HEAD（可选，见下）
gbl.Options("/items", myOptions)           // OPTIONS（可选，见下）
```

### `Head` 自动兜底

注册 `Head` 是可选的：若某路径**没有注册** `HEAD`，它自动复用同路径的 `GET` 规则。

```go
// 只注册 GET，但 HEAD /health 仍然 200（只有 header）。
gbl.Get("/health", func() string { return "ok" })

// 客户端：HEAD /health
// 响应：200 OK  Content-Type: text/plain  Content-Length: 2
// （net/http 会丢弃 body，但保留 header 与 Content-Length。）
```

### `Options` 自动兜底

若某路径**没有注册** `OPTIONS`，框架会以 `204 No Content` + `Allow` 头（列出该路径实际支持的方法）自动应答；注册了 handler 就等于该路径关闭自动应答。

```go
// 该路径上只注册 POST。
gbl.Post("/items", createItem)

// 客户端：OPTIONS /items
// 响应：204 No Content  Allow: POST

// 一旦注册 OPTIONS，就由你的 handler 接管应答：
gbl.Options("/items", func(w http.ResponseWriter) {
    w.Header().Set("Allow", "POST, OPTIONS")
    w.WriteHeader(http.StatusNoContent)
})
```

### 有 body 的方法 vs 无 body 的方法

`POST`/`PUT`/`PATCH` 按 `Content-Type` 读取并装配 body；`GET`/`HEAD`/`DELETE`/`OPTIONS` 从不读 body，客户端就算塞了 body 也会被丢弃，因此只有路径和 query 参数会装配。

```go
// DELETE 只读路径/query。
gbl.Delete("/items/:id", func(id string, hard bool) string {
    // id 来自 :id 路径段，hard 来自 ?hard=true
    return fmt.Sprintf("delete %s hard=%v", id, hard)
})

// 客户端：DELETE /items/7?hard=true   （body 会被忽略）
// handler 收到：id="7", hard=true
```

---

## 4. 分组（`Group` / `RouteGroup`）

`Group(prefix string) RouteGroup` 返回一个**带路径前缀、带分组作用域中间件**的注册句柄。它的方法与包级动词一一对应，只是给每个路径加上前缀、给经它注册的规则附上分组中间件；**每个方法都返回分组自身**，因此可链式调用。

```go
api := gbl.Group("/api")

// 链式：每次注册都返回分组。
api.
    Get("/ping", func() string { return "pong" }).        // /api/ping
    Post("/ping", func() string { return "posted" })      // /api/ping

// 分组中间件只作用于经本分组注册的规则。
api.Use(func(c *gbl.Context) {
    if c.Request.URL.Query().Get("token") == "" {
        c.AbortWithStatusString(http.StatusUnauthorized, "missing token")
        return
    }
    c.Next()
})
api.Get("/secret", func() string { return "top" })        // /api/secret
```

`RouteGroup` 接口：

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

> 分组中间件有作用域：`Group("/admin").GroupUse(...)` 只给**通过该分组注册的路由**跑，不会泄到同级的其他路由，也不会进引擎级 `Use` 链。

```go
// 全局中间件：所有路由都会打这一行。
gbl.Use(func(c *gbl.Context) {
    log.Println("global")
    c.Next()
})

// admin 分组专用守卫：只有 /admin/* 路由会跑它。
admin := gbl.Group("/admin")
admin.GroupUse(func(c *gbl.Context) {
    if c.Request.URL.Query().Get("token") != "secret" {
        c.AbortWithStatusString(http.StatusUnauthorized, "invalid token")
        return
    }
    c.Next()
})
admin.Get("/stats", func() string { return "stats" })   // global + admin 守卫

// 同级另一条路由，没有走 admin 分组：
gbl.Get("/public", func() string { return "public" })   // 只有 global

// 另一个分组也不会沾到 admin 守卫：
user := gbl.Group("/user")
user.Get("/profile", func() string { return "profile" }) // 只有 global
```

请求 `/admin/stats?token=wrong` 时返回 401；请求 `/public` 或 `/user/profile` 时**照常 200**，因为 admin 守卫根本不在它们的链里。

---

## 5. 中间件（`Use` / `HandlerFunc`）

```go
type HandlerFunc func(c *Context)
```

`HandlerFunc` 是洋葱链上的一环。整条链是一个有序切片——引擎级 `Use`（最外）→ 分组 `Use` → … → 匹配到的 handler（最内），`Context` 里保存着一个指向当前环的游标。

**`c.Next()` 会执行当前环之后的所有环**（剩余中间件，最后是 handler），然后返回。因此 `c.Next()` 之前是**请求阶段**，之后是**响应阶段**，响应阶段按由内向外回溯。框架启动时调一次 `Next()` 拉起整条链；每个中间件再调它把控制权交给下游。

这两个阶段都是**你在中间件函数里自己写的代码**，以 `c.Next()` 这一行为界——它们不是 handler，也不是框架写 HTTP 响应那个动作。handler 是最内层的业务函数：没有阶段之分，也从不叫 `Next()`。“响应”指的是出站方向（handler 之后），不是写字节这个动作：框架要等整条链回溯完，才把暂存 body 写进连接。所以围着 `Next()` 计时测的是请求处理耗时（下游 + handler + 序列化）——访问日志也放在这一阶段，因为此时结果与状态码已确定。

`Next()` 就是一个普通的阻塞调用，不是什么调度器：它同步跑完下游所有环，等它们全部结束才返回。所谓”响应阶段”，不过是中间件自己在 `Next()` 调用之后的代码——因为调用返回了，自然就接着执行，`Next` 内部并没有第二套机制。以链 `[A, B, handler]` 为例，执行就是沿下图自上而下流动：

```text
请求
  │
  ▼
  A.Next() 之前            ┐
  │                        │ 请求阶段
  ▼                        │ （每调一次 Next() 深入一层）
  B.Next() 之前            ┘
  │
  ▼
  handler                  ── 执行业务，暂存结果
  │
  ▼
  B.Next() 之后            ┐
  │                        │ 响应阶段
  ▼                        │ （Next() 逐层返回，向外回溯）
  A.Next() 之后            ┘
  │
  ▼
写回暂存结果
  │
  ▼
响应
```

**`Next()` 的内部机制**：每次调用 `c.Next()` 都会递增一个共享的游标，然后循环调用剩余的 handler。当中间件调用 `c.Next()` 时，它会**递归地再次进入** `Next()` 函数，创建新的栈帧。所有嵌套调用共享同一个游标（`c.index`），最内层的调用把游标推过末尾，导致每个外层循环依次退出。这就是为什么没有 handler 会被执行两次，以及为什么响应阶段按反向顺序（由内向外）回溯。

所有嵌套的 `Next` 共享同一个游标；最内层调用把游标推过末尾，外层各帧便依次返回，任何环都不会被执行两次。

**要中断链，必须调 `Abort` 系列方法**（`Abort`、`AbortWithStatusString`、`AbortWithStatusJSON`）：它把游标推到末尾之后，**跳过所有尚未开始的环**（含 handler）。`AbortWithStatusString` / `AbortWithStatusJSON` 还会同时写拒绝响应，一步到位。

> 中间件**不调 `c.Next()` 直接 `return` 并不能中断链**：外层的 `Next` 循环仍会推进到下一环。只有 `Abort` 才能拦下下游。

> **`Abort` 只向下游中断，不会中断上游。** 已经调用过 `c.Next()` 的中间件，即使下游被 Abort 了，它们的 `c.Next()` 仍会返回，返回后的代码仍会执行。这是设计意图：访问日志、指标统计和清理代码必须无论请求成功还是被拒绝都要运行。

```go
// 示例：下游中间件 Abort
gbl.Use(func(c *gbl.Context) {
    log.Println("mw1: Next 之前")
    c.Next()
    log.Println("mw1: Next 之后")  // ← 即使 mw2 Abort 了，这行仍会执行
})

gbl.Use(func(c *gbl.Context) {
    log.Println("mw2: Abort 之前")
    c.AbortWithStatusString(401, "unauthorized")
    log.Println("mw2: Abort 之后")  // ← 这行不会执行（在 Abort 之后）
    // mw2 返回，Next() 返回到 mw1
})

gbl.Get("/api", func() string {
    log.Println("handler")  // ← 这行不会执行（被 Abort 跳过）
    return "secret"
})

// 请求 GET /api 的日志输出：
//   mw1: Next 之前
//   mw2: Abort 之前
//   mw1: Next 之后        ← Next 之后的代码总是执行
// 响应：401 unauthorized
```

**要在 Abort 时跳过 Next 之后的代码**，检查 `c.Aborted()`：

```go
gbl.Use(func(c *gbl.Context) {
    c.Next()
    if !c.Aborted() {
        log.Println("成功路径")  // ← 只在未 Abort 时执行
    } else {
        log.Println("中断路径")  // ← 只在 Abort 时执行
    }
})
```

> **为什么 Next 之后的代码总是执行？** `c.Next()` 是一个普通的函数调用，会创建栈帧。当下游中间件 Abort 时，它只是设置一个标志（`index = abortIndex`）然后返回，但上游的 `Next()` 调用已经在栈上，必须等待函数返回。这是 Go 的正常函数调用行为——除了 `panic` 之外没有办法强制退出栈帧，而 panic 不适合用于控制流。

**这是设计意图，不是限制。** Next 之后的代码必须无论成功还是失败都执行：

```go
// 访问日志：每个请求都需要日志记录，包括被拒绝的
gbl.Use(func(c *gbl.Context) {
    start := time.Now()
    c.Next()
    // 即使 Abort 了这里也会执行，所以你可以记录 401/403 拒绝
    log.Printf("%s %s %d %v", 
        c.Request.Method, 
        c.Request.URL.Path,
        c.StatusCode(),  // ← Abort 时是 401，成功时是 200
        time.Since(start))
})

// 认证中间件
gbl.Use(func(c *gbl.Context) {
    if !authenticated {
        c.AbortWithStatusString(401, "unauthorized")
        return
    }
    c.Next()
})

// 请求：GET /api（未认证）
// 流程：logger(before) → auth → Abort(401) → logger(after: 记录 401)
// 输出：GET /api 401 1.2ms  ← 被拒绝的请求仍然有日志
```

如果 Abort 强制退出整个链，日志中间件的 Next 之后的代码就永远不会执行，你会丢失所有被拒绝请求的日志——无法监控认证失败率或审计安全事件。

**灵活性：由你选择执行什么。** 用 `c.Aborted()` 来条件性执行 Next 之后的逻辑：

```go
gbl.Use(func(c *gbl.Context) {
    c.Next()
    
    // 总是执行：所有请求的指标统计（成功 + 失败）
    metrics.Record(c.StatusCode(), c.Size())
    
    // 只在成功时执行：缓存结果
    if !c.Aborted() {
        cache.Store(key, c.Result())
    }
    
    // 只在 Abort 时执行：审计拒绝事件
    if c.Aborted() {
        audit.LogRejection(c.Request, c.StatusCode())
    }
})
```

**业界标准。** Gin（最流行的 Go web 框架）采用完全相同的设计：Abort 阻止下游，但上游中间件的 Next 之后的代码总是执行。这是 Go web 框架中间件链的标准模式。

```go
// 一个中间件：函数体被 c.Next() 分成两段。
gbl.Use(func(c *gbl.Context) {
    // ── 请求阶段：往下游走之前 ──
    start := time.Now()
    c.Set("requestID", newRequestID())

    c.Next() // 先跑完下游所有中间件 + handler，然后才返回

    // ── 响应阶段：此时 handler 已经跑完 ──
    log.Printf("耗时 %s, 状态 %d", time.Since(start), c.StatusCode())
})

// handler 只是业务逻辑：没有阶段之分，也不调 Next()。
gbl.Get("/ping", func() string { return "pong" })

// gbl.Use 是变参，可一次装多环：
gbl.Use(recoverer, requestID)
```

中间件链执行顺序：引擎级 `Use`（最外）→ 分组 `Use` → … → 匹配到的 handler（最内）。各环"`c.Next()` 之后"的半段按**由内向外**回溯执行。

```go
// 展示链的回退顺序。
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

// 请求 GET /api/ping，日志依次输出：
//   engine before
//   group before
//   handler
//   group after
//   engine after
```

### 请求的完整生命周期

从请求到达到响应回到客户端，逐步发生的事：

1. **接收与分发。** `net/http` 接受连接、解析请求，调用 server 的 handler——它就是引擎本身；若调过 `SetCorsConfig`，则是包着引擎的 `corsHandler`。
2. **CORS（仅配置时）。** `corsHandler` 检查 `Origin`：预检直接应答；允许的来源加上 CORS 头；随后交给引擎。无 `Origin` 则直接进引擎。
3. **方法与路由匹配。** 引擎解析动词（未知 → 501），在该方法的路由树里匹配并填充路径参数。未注册的 `HEAD` 借用 `GET` 路由。未匹配 → 404，或 405 + `Allow`（`OPTIONS` 则 204）。
4. **构建 Context。** 从池中取一个 `Context`，装入请求、匹配到的路径参数、该路由冻结的链与 logger。链为 `[全局中间件…, 分组中间件…, 终端 endpoint]`。
5. **执行链 —— `c.Next()`。** 链是一个 `HandlerFunc` 切片；它的最后一环不是业务 handler 本身，而是 `routeEndpoint`——一个普通中间件，函数体只有一句 `c.invokeHandler()`：

```go
// 每条链的最后一环（context.go）
var routeEndpoint HandlerFunc = func(c *Context) { c.invokeHandler() }

// 启动时组装（route.go）：全局中间件 + 分组中间件 + routeEndpoint
chain = append(chain, e.middlewares...)
chain = append(chain, meta.mw...)
chain = append(chain, routeEndpoint)
```

   这条链是**启动时由 `generateRoute` 一次性建好**的：调 `Get`/`Post`/… 注册路由只是把它排队；`Run()` 才组装链并冻结进该路由的 `routeHandler.handlers`，`routeEndpoint` 就是在这时作为最后一环 append 上去的。每个请求都复用这条冻结的切片（`newContext` 里 `c.handlers = rh.handlers`），不会按请求重新组装。

   `Next()` 就是循环调用这个切片里的每一环。调到 `routeEndpoint` 时，就执行了 `invokeHandler()`：解析、装配、校验、反射调用业务 handler（存在 `c.rh` 里）、序列化结果。洋葱嵌套如下：

```text
engine.Next() → handlers[0]
└─ 全局中间件
   ├─ 请求阶段代码
   ├─ c.Next() → handlers[1]
   │  └─ 分组中间件
   │     ├─ 请求阶段代码
   │     ├─ c.Next() → handlers[2]
   │     │  └─ routeEndpoint
   │     │     └─ c.invokeHandler()：
   │     │        解析 → 装配 → 校验 → 调用 handler → 序列化 c.bytes
   │     └─ 响应阶段代码（Next 返回后）
   └─ 响应阶段代码（Next 返回后）
```

6. **写回响应。** 若中间件或 handler 已写过 body（`c.JSON`、`c.String` 或注入的 `http.ResponseWriter`），就以它为准。否则引擎用注册时选定的 Content-Type 写出暂存的 `c.bytes`，先 flush 已记录的状态码（默认 200）。
7. **收尾。** 清理 multipart 临时文件；`Context`、路径参数 map、响应写入器复位并归还对象池。
8. **任何 panic** 都会被恢复并渲染：框架自身生成的中止原样到达客户端；注册了 `PanicProcessor` 则由它接管渲染；其余一律记日志并回通用 500。

handler 本身（第 5 步最内层）是纯业务代码：既不调 `Next()`，也没有阶段之分。“响应阶段”属于中间件、在回程上——所以计时和访问日志在那里读结果。

---

## 6. 服务启停（`Run` / `Shutdown`）

### `Run()`

在配置的地址上启动这唯一的引擎并**阻塞**直到停止。收到 `SIGINT`/`SIGTERM` 时执行优雅停机（关闭监听、给在途请求收尾窗口）。任何致命启动错误都会 panic。

```go
gbl.Get("/hi", func() string { return "hi" })
gbl.Run() // 端口来自 Configure 覆盖，默认 :9801
```

> **服务期间引擎只读。** `Run` 会冻结全部装配期决定——路由注册、`Use`、
> `SetLogger`、`Configure`、CORS、大小上限、panic 渲染器。在 `Run` 之后调用
> 它们（或重复调用 `Run`）会 panic 并给出明确的 *"必须在 Run 之前调用"*
> 提示，而不是被悄悄吞掉。

### `Shutdown(ctx context.Context) error`

优雅停止由 `Run` 启动的服务，等待在途请求直到 `ctx` 到期。可重复调用，也可从**不同于 `Run`** 的 goroutine 调用。在 `Run` 之前调用会返回 `http.ErrServerClosed`。

```go
// 典型用法：从另一个 goroutine（如某个管理接口）触发停机。
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
if err := gbl.Shutdown(ctx); err != nil {
    log.Fatal(err)
}
```

> `Run` 已自带信号优雅停机；当你需要*主动*触发（健康检查 kill 开关、测试、嵌入）时才用 `Shutdown`。

---

## 7. `Context` 详解

`*Context` 贯穿整条中间件链，当 handler 声明它时会被作为参数注入。它携带请求、匹配到的路径参数，以及一个用于在链路与 handler 之间传递数据的 key/value 袋子；响应经 `c.JSON` / `c.String` / `c.Status` 写出。

> **`Context` 是池化复用的对象，只在本次请求内有效。** 请求一结束，它就会被清空并交给下一个请求复用。因此别把 `*Context` 存进全局变量或缓存，也别在请求结束后才运行的 goroutine / 回调里捕获它——那样读到的是别的请求的数据或零值，并发访问还会造成数据竞争。需要异步或跨请求使用的数据，请在请求结束前把用到的值（如 `c.PathParam`、`c.Get` 的结果）拷贝进你自己的变量或结构体。注入的 `http.ResponseWriter` 同样池化，规则一致，不得跨请求持有。

导出字段：

```go
c.Request // *http.Request —— 原始入站请求
```

### 方法

| 方法 | 用途 |
|---|---|
| `Next()` | 执行链上剩余部分（到末端即 handler） |
| `PathParam(name) string` | 按名字取匹配到的路径参数（供中间件用；业务 handler 请按名绑定） |
| `Set(key, value)` / `Get[T](key) (T, bool)` / `Value[T](key) T` | 请求级取值（见 §8） |
| `Result() any` | handler 返回值的 `any` 形态（`Next()` 之后有效）；首次读取时才做装箱 |
| `IsWritten() bool` | body 是否已提交 |
| `StatusCode() int` | 将要/已经发送的状态码（默认 200） |
| `Size() int` | 有效响应体长度 |
| `Aborted() bool` | 链路是否已中断 |
| `Abort()` | 中断链路，不写任何内容 |
| `AbortWithStatusString(status int, body string)` | 以纯文本 body 拒绝 |
| `AbortWithStatusJSON(status int, body any)` | 以 JSON body 拒绝 |

### 示例

```go
// 场景：很多接口都要知道“当前登录用户是谁”。请求带 Authorization: Bearer <token>。
// 鉴权是横切逻辑——几十个接口共用，且未登录必须在进 handler 前统一拦下——
// 所以放在一个中间件里做一次：把 token 解析成 uid。
gbl.Use(func(c *gbl.Context) {
    uid, ok := parseToken(c.Request.Header.Get("Authorization")) // 你的解析逻辑
    if !ok {
        c.AbortWithStatusString(http.StatusUnauthorized, "unauthorized")
        return
    }
    c.Set("uid", uid)
    c.Next()
})

// handler 不碰 token，只用当前用户。uid 来自 token，而非 path/query/body，
// 自动绑定给不了——Context 是中间件传给 handler 的唯一通道。
gbl.Get("/orders", func(c *gbl.Context) []Order {
    uid, _ := c.Get[int64]("uid")
    return loadOrders(uid)
})

// 场景：接口监控——统一记录每个请求的状态码、响应大小与耗时。
// handler 跑完后，只有 Context 能拿到暂存返回值与有效状态/大小；
// 注入的 http.ResponseWriter 只能写，读不到这些。
gbl.Use(func(c *gbl.Context) {
    c.Next()
    metrics.Observe(c.StatusCode(), c.Size())
    if c.Aborted() {
        log.Printf("请求被中断，状态码 %d", c.StatusCode())
    }
})
```

---

## 8. 请求级取值（`Get` / `Value`）

中间件用 `c.Set` 存值，handler（或后续环）用泛型 `c.Get[T]` 读取：断言由框架代劳，键不存在或类型不符都返回（零值, false），由调用者决定怎么处理：

```go
gbl.Use(func(c *gbl.Context) {
    c.Set("uid", int64(7))
    c.Next()
})

gbl.Get("/me", func(c *gbl.Context) string {
    uid, ok := c.Get[int64]("uid")
    if !ok {
        return "anonymous"
    }
    return fmt.Sprintf("user %d", uid)
})
```

缺失本来就可以当零值用时，用 `c.Value[T]` 省掉 ok；不想指定类型、原样取回存的值，用 `Get[any]`：

```go
name := c.Value[string]("displayName") // 缺失/类型不符为 ""
v, ok := c.Get[any]("uid")             // int64(7), true —— 原样返回
```

---

## 9. 中止与 panic（`PanicProcessor` / `HttpStatus`）

gbl 的错误策略按"**消息是谁写的**"划分回显权：**应用开发者写的消息原样到达客户端**——（若你装了）你的 `PanicProcessor`、以及 `Validate` 返回的错误文本；**框架从内部拼出的消息永不回显**——非预期 panic 的原因、绑定失败的反射细节（参数名、Go 类型、strconv 详情），都只进服务端日志，客户端只拿通用 `500` / `400`。请求解析错误（坏 Content-Type、body 超限等）描述的是客户端自己的请求帧，不含内部标识，故原样回显。

### `PanicProcessor` / `RegisterPanicProcessor` / `HttpStatus`

```go
type PanicProcessor func(err any) (any, HttpStatus)
type HttpStatus int
```

`RegisterPanicProcessor(p PanicProcessor)` 在默认引擎上安装你的 panic 渲染器。**一旦注册，它就接管所有被恢复值的渲染**——显式中止*和*真实崩溃都归它，因此你要为"不泄露内部错误"负责。返回的 body 可以是任意可渲染值（`string`、`error`、`struct`、`map`……）。

```go
gbl.RegisterPanicProcessor(func(err any) (any, gbl.HttpStatus) {
    // 返回统一、安全的信封；绝不要把 err 拼进 body。
    return map[string]any{"code": 500, "message": "internal error"}, gbl.HttpStatus(500)
})
```

---

## 10. 校验（`Validatable`）

```go
type Validatable interface { Validate() error }
```

任何被装配的**业务参数**（含指针），只要其类型实现了 `Validate() error`，就会被自动校验，时机是**装配之后、handler 之前**。没实现的类型零开销。

```go
type Signup struct {
    Email string
    Age   int
    Name  string
}

// 每个类型只写一个 Validate——所有字段的规则都在这一个方法里。
// gbl 不强加任何自己的规则：用纯 Go，或调你自己的校验库。
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

// handler 无需判空；Validate 返回非 nil 错误即 400，错误文本原样回显。
gbl.Post("/signup", func(s Signup) string {
    return "signed up: " + s.Email
})
```

非 nil 错误会以 **400** 中断请求，**错误文本原样作为响应体**——消息是你自己写的，回显与否的所有权在你：想给客户端看什么就写什么，不想暴露的就别写进 `Validate`。框架侧的绑定失败则相反：只回通用 `400`，反射细节进服务端日志（见第 9 节）。

---

## 11. 日志（`Logger` / `BannerLogger` / `SetLogger` / `Info` / `Warn` / `Error`）

两个来源，一个出口。框架内部产生的每一条消息（恢复的 panic、绑定失败、启动中止），以及业务代码经 `gbl.Info` / `gbl.Warn` / `gbl.Error` 写的每一行日志，都流经引擎上装配的那一个 `Logger`——发往哪里、什么格式、带不带颜色，全由这个 sink 说了算。

### 默认控制台（开发辅助）

没有 `SetLogger` 之前，框架与门面日志打印到控制台：

```
2026-11-20 16:12:38 INFO started port=9801
```

* 形态：标准日期/时间前缀之后的 `LEVEL msg key=value ...`。
* 真终端上级别词着色——`INFO` 绿、`WARN` 黄、`ERROR` 红。管道、重定向、CI 下保持纯文本；设置环境变量 `NO_COLOR` 可在真终端上主动关闭颜色。
* 默认 sink 是一个**私有** `log.Logger`，从不借用标准库的全局 logger：你的 `log.SetOutput` / `log.SetFlags` 移动不了、也改不了框架日志的样式；框架日志同样不会捕获或染色你自己的 `log.Println`——两条通道完全隔离。

### `Logger`

```go
type Logger interface {
    Info(msg string, keysAndValues ...any)
    Warn(msg string, keysAndValues ...any)
    Error(msg string, keysAndValues ...any)
}
```

`msg` 描述事件，其后是交替的 key/value。结构化后端（zap/slog）可直接消费；默认实现保持纯文本形态。这个接口就是**唯一的集成契约**：实现它、装上它，就完了。

### `SetLogger`

```go
gbl.SetLogger(myLogger)   // 传 nil 会被忽略，框架始终保留一个可用 logger
```

和所有装配 API 一样，必须在 **`Run` 之前**调用（服务期间引擎只读，见 §6）。接 zap/slog/zerolog 就是一个薄适配器：把 `msg` 与键值对转发给你的 logger 实例——至于长什么样（级别词进不进正文）由那些组件决定。

### `Info` / `Warn` / `Error`（业务入口）

```go
gbl.Get("/user", func(id string) string {
    gbl.Info("user loaded", "id", id) // → 引擎当前装的 Logger
    return fetch(id)
})
```

包级三函数是给业务代码的人体工学入口：`SetLogger` 注入之后，那就是框架的出口，直接调框架 API 即可复用，无需自己保存或取回任何引用。框架内部事件走的是同一个出口，所以装一次，所有日志的去向就都定了。

### `BannerLogger`（可选）

```go
type BannerLogger interface { Banner(block string) }
```

一个*同时*实现了 `BannerLogger` 的 `Logger`，会拿到预渲染成多行的启动横幅块，可原样输出，不必让时间戳/级别标签挤乱 ASCII art。只实现 `Logger` 的也照样能收到横幅，只是作为普通 `Info` 记录。

```go
type appLogger struct{ ... }

func (l appLogger) Info(msg string, kv ...any) { ... }
func (l appLogger) Warn(msg string, kv ...any) { ... }
func (l appLogger) Error(msg string, kv ...any) { ... }

func (l appLogger) Banner(block string) {   // 可选伴随接口
    fmt.Println(block)                       // 干净打印 art，无前缀
}

gbl.SetLogger(appLogger{})
```

### API 监控归你

框架**有意**不带访问日志中间件。监控 API 调用情况是应用层面的事，而写一行中间件所需的信息链路上全都有——自己装一个：

```go
gbl.Use(func(c *Context) {
    start := time.Now()
    c.Next()
    gbl.Info("access", "path", c.Request.URL.Path,
        "status", c.StatusCode(), "size", c.Size(), "cost", time.Since(start))
})
```

### 文件日志（集成示例）

框架本身**只打印控制台**。要写文件，对着任意 writer 实现接口即可——比如下面这个指向文件的标准库 `log` 版本：

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

日志轮转、按级别分文件、控制台与文件双写等更完整的能力，交给 zap/slog 适配器或未来的 gbl-boot 集成层——内核不内置文件日志。

---

## 12. 上传大小限制（`ByteSize` / `SetMaxBodySize` / `SetMaxFileSize` / `SetMaxUploadFileSize`）

```go
type ByteSize int
```

`ByteSize` 是一个具名 `int`，代表字节数 —— 类型名本身就在调用点说明了参数含义。本节讲的是**上传文件**：文件只在携带 `multipart/form-data` 的体方法上装配，文件参数按“字段名与参数名相同的那个文件 part”匹配（精确匹配）。这条文件查找路径与文本 form/query 参数是两条独立的来源，互不相干；支持的参数类型见下表。

```go
import "mime/multipart"

// 表单字段 "avatar" 下的单个文件。
gbl.Post("/upload", func(avatar *multipart.FileHeader) string {
    return fmt.Sprintf("%s (%d bytes)", avatar.Filename, avatar.Size)
})

// 同一字段 "photos" 下的多个文件。
gbl.Post("/gallery", func(photos []*multipart.FileHeader) string {
    return fmt.Sprintf("%d files uploaded", len(photos))
})
```

| 参数类型 | 装配 |
|---|---|
| `*multipart.FileHeader` / `multipart.FileHeader` | 匹配字段下的第一个文件 |
| `[]*multipart.FileHeader` / `[]multipart.FileHeader` | 匹配字段下的所有文件 |

没有上传时指针为 `nil`、切片为空——不会 panic。

### `SetMaxBodySize` / `SetMaxFileSize` / `SetMaxUploadFileSize`

```go
func SetMaxBodySize(size ByteSize)          // 整个请求 body 的硬上限（超限返回 413）
func SetMaxFileSize(size ByteSize)          // multipart 单文件内存阈值；更大的落临时文件
func SetMaxUploadFileSize(size ByteSize)    // 每个上传文件的单文件硬上限（超限返回 413）
```

```go
gbl.SetMaxBodySize(gbl.ByteSize(32 << 20))       // 总 body 上限 32 MiB
gbl.SetMaxFileSize(gbl.ByteSize(4 << 20))        // 超过 4 MiB 的文件落临时文件
gbl.SetMaxUploadFileSize(gbl.ByteSize(5 << 20))  // 允许单个文件最大 5 MiB（默认 1 MiB）
```

三个限制是**互相独立**的开关，各自回答一个不同的问题。三者都只接受正的 `ByteSize`，传非正值即 panic；下表的**默认值**一栏是启动时生效、直到你覆盖它为止的值。

| 设置 | 控制什么 | 超限后果 | 默认值 |
|---|---|---|---|
| `SetMaxBodySize` | **整个请求 body 的总大小**（含 multipart 上传） | 以 `413` 拒绝 | **5 MiB** |
| `SetMaxFileSize` | multipart 解析的**内存阈值**——不超过此大小的 part 留在内存，更大的落临时文件 | 无；它*不是*上限、从不拒绝请求 | **2 MiB** |
| `SetMaxUploadFileSize` | **每一个单独上传文件的最大大小**（任意字段、任意 part） | 进入 handler 前以 `413` 拒绝 | **1 MiB** |

三个一起理解：

- `SetMaxBodySize` 是唯一针对**整体**请求的限制——不管 body 拆在多少字段上，总量过大就被拒。
- `SetMaxFileSize` 纯是个**性能**设置（内存 vs 磁盘）；它**不**限制你能上传多少。
- `SetMaxUploadFileSize` 是唯一的**单文件**闸门：即使总量仍在 `SetMaxBodySize` 之内，只要某一个文件过大也照样拒绝（表单解析后逐个按 `FileHeader.Size` 检查）。它的 1 MiB 默认值**低于** 5 MiB 的整 body 默认值，所以开箱即用就会生效：任何单个文件不得超过 1 MiB，但整体 body 仍可容纳最多 5 MiB 的多个小文件。大文件上传对服务器有风险，所以这道上限默认就强制拦截——只有确实需要接收更大文件时才调高它。

---

## 13. 跨域（`SetCorsConfig` / `CorsConfig`）

```go
func SetCorsConfig(cc *CorsConfig)   // 传 nil 关闭 CORS
```

`SetCorsConfig` 在默认引擎上安装跨域策略，`Run` 随后用它包裹处理链——预检、来源/方法/头匹配、凭据与 `max-age`。`CorsConfig` 直接用普通结构体字面量构造，字段全部导出：

```go
type CorsConfig struct {
	AllowedOrigins   []string      // 允许的来源（Access-Control-Allow-Origin），或 "*"
	AllowedMethods   []string      // 允许跨域携带的 HTTP 方法
	AllowedHeaders   []string      // 预检中接受的请求头
	ExposedHeaders   []string      // 允许跨域客户端读取的响应头
	AllowCredentials bool          // 是否允许跨域携带凭据（cookie/认证）
	MaxAge           time.Duration // 客户端可缓存预检结果的时长
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

// 任意站点都可读接口——但不带凭据：
gbl.SetCorsConfig(&gbl.CorsConfig{
	AllowedOrigins: []string{"*"},
	AllowedMethods: []string{http.MethodGet},
})

gbl.SetCorsConfig(nil) // 重新关闭 CORS
```

一条**安全策略**会拦截危险的组合——例如 `AllowCredentials=true` 同时 `AllowedOrigins` 包含通配 `"*"`，这等于允许任意站点发起携带凭据（cookie/认证）的跨域请求。命中这条策略的配置会在 `Run` 之前中止启动。

---

## 14. 配置（`Config` / `Configure`）

内核**不读任何配置文件**：默认值开箱即用，要改就在代码里覆盖。配置文件（YAML、环境变量、profile、配置中心）属于集成层的职责——未来的 gbl-boot 会解析这些来源，然后调用同一个 `Configure` 入口。

```go
type Config struct {
    Name              string        // 应用名
    Port              string        // 监听端口，不带前导冒号
    CertFile          string        // TLS 证书文件路径
    KeyFile           string        // TLS 私钥文件路径
    ReadTimeout       time.Duration // 读完整个请求的最大时长
    ReadHeaderTimeout time.Duration // 读完请求头的最大时长
    WriteTimeout      time.Duration // 写完整个响应的最大时长
    IdleTimeout       time.Duration // keep-alive 连接的最大空闲等待
}
```

### `Configure`

```go
func Configure(fn func(*Config))
```

`fn` 拿到的是**已填好默认值**的配置，只赋值你关心的字段即可。在 `Run` 之前调用；传 nil 是空操作。

```go
gbl.Configure(func(c *gbl.Config) {
    c.Name = "myapp"
    c.Port = "8080"
    c.ReadTimeout = 5 * time.Second
})
```

| 字段 | 默认 | 含义 |
|---|---|---|
| `Name` | `gbl` | 应用名 |
| `Port` | `9801` | 监听端口 |
| `CertFile` / `KeyFile` | 空 | 两者都给 ⇒ 启用 HTTPS |
| `ReadTimeout` / `ReadHeaderTimeout` / `WriteTimeout` / `IdleTimeout` | 10s / 5s / 10s / 60s | 服务端超时 |

**TLS**：`CertFile` 与 `KeyFile` 同时非空即启用 HTTPS，每次 `Configure` 后自动重新判定。路径原样交给标准库，**相对路径相对的是进程启动时的工作目录**，不是可执行文件所在目录；部署时工作目录不可控的话，在 `Configure` 里自行拼出绝对路径。

> 业务自己的配置（数据库连接等）由应用自理——内核只管服务运行参数。需要文件/环境配置时，在应用里解析后调 `Configure`，或等 gbl-boot。

---

## 15. 启动失败（fail-fast）

所有启动、注册类失败都是 **fail-fast**：gbl 先经引擎 `Logger` 记录原因、再 panic——还没开始服务就挂掉的部署，也会在你的日志 sink 里留下原因，而不只是一条裸栈。端口占用、路由冲突、危险的 CORS 策略，以及参数名在运行时不可得的按名字装配 handler（用 `-ldflags "-w"` 或 `"-s"` 构建、剥除了 gbl 读取参数名所依赖的 DWARF 的二进制）都走这条路。

日志默认只在控制台；装了自定义 `Logger` 就落在你的 sink 里。内核不写任何兜底文件。

---

## 16. 响应体压缩（`Compress`）

`Compress(level int) HandlerFunc` 返回一个中间件，在有益时对响应体做 gzip 压缩。压缩发生在 `c.Next()` 返回之后，此时 handler 已产出结果。

**三个条件必须同时成立：**

1. 客户端发送了 `Accept-Encoding: gzip`
2. 响应的 `Content-Type` 是可压缩的（文本、JSON、XML、CSS、JS、SVG 等）
3. 响应体至少 **1 KB**——更小的响应压缩收益甚微，CPU 开销不值

三个条件都成立时，中间件将暂存的响应体替换为压缩数据，并设置 `Content-Encoding: gzip` 和 `Vary: Accept-Encoding`。

**压缩级别**（与 `compress/gzip` 一致，重新导出，无需直接 import 标准库）：

| 常量 | 值 | 含义 |
|------|----|------|
| `NoCompression` | 0 | 不压缩 |
| `BestSpeed` | 1 | 最快 |
| `DefaultCompression` | -1 | 均衡（推荐默认） |
| `BestCompression` | 9 | 最小输出，最高 CPU |
| `HuffmanOnly` | -2 | 仅 Huffman 编码 |

**全局使用：**

```go
package main

import "github.com/gblgo/gbl"

func main() {
    gbl.Use(gbl.Compress(gbl.DefaultCompression))
    gbl.Get("/api/data", handler)
    gbl.Run(":8080")
}
```

**分组级使用：**

```go
api := gbl.Group("/api")
api.GroupUse(gbl.Compress(gbl.BestSpeed))
api.Get("/data", handler)
```

**中间件顺序：** Compress 在 `Next()` 之后执行，观察到的是最终响应。应放在 auth/CORS 中间件**之后**（错误响应跳过压缩），放在日志中间件**之前**（访问日志如需报告压缩后大小可读到）。典型顺序：

```go
gbl.Use(
    logger,            // 最外层：看到所有
    corsMiddleware,    // 处理预检
    authMiddleware,    // 拒绝未认证
    gbl.Compress(gbl.DefaultCompression), // 压缩成功响应
)
```

**二进制格式永不压缩：** 图片、视频、音频、PDF 等二进制内容类型会自动跳过，因为它们已经压缩过或 gzip 无收益。

---

## 性能说明

热路径已用预计算的装配计划、对象池（响应写入器 / 路径参数 / `Context` / `Context.keys`）和无分配的路由匹配优化过。
静态路由（无路径参数）使用 O(1) map 查找代替基数树。
与成熟框架的剩余差距来自反射调用任意 handler 签名的固有代价——这是这一核心特性带来的取舍，不是轻易能抹平的。

---

## License

[MIT](./LICENSE) © lutme
