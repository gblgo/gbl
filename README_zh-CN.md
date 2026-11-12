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
  <b>gbl</b> 是一个 <b>零依赖</b> 的 Go Web 框架：用 Go 标准库即可跑起一个完整的 HTTP 服务，
  无需任何第三方包。它的招牌是——<b>handler 签名随便写，框架自动帮你把参数装配好、把返回值序列化好</b>。
</p>

---

## 目录
1. [框架功能介绍](#1-框架功能介绍)
2. [简单的使用示例](#2-简单的使用示例)
3. [对外提供的拓展能力](#3-对外提供的拓展能力)
4. [配置参考（application.ini）](#4-配置参考applicationini)
5. [安装与运行](#5-安装与运行)

---

## 1. 框架功能介绍

| 能力 | 说明 |
|---|---|
| **零依赖** | `go.mod` 不引入任何第三方包，只用标准库。体积小、无供应链负担。 |
| **任意签名 handler** | 参数按**类型 + 名字**自动绑定：路径参数、query、JSON/form 请求体、`*Context`、`*http.Request`、`http.ResponseWriter`、上传文件；无需固定签名。 |
| **返回值自动序列化** | 直接 `return` 业务值：`string`/标量按文本输出，`struct`/`map`/切片自动转 JSON，`nil` 输出空体。 |
| **路由树匹配** | 静态段优先、动态段（`:id`）回退；每个 HTTP 方法独立一棵树。 |
| **HTTP 方法全覆盖** | `GET / HEAD / POST / PUT / PATCH / DELETE / OPTIONS`，以及 `Any` / `Match`。未注册 `HEAD` 自动借用 `GET`；未注册 `OPTIONS` 由框架回 `204 + Allow`。 |
| **中间件洋葱链** | 全局 `Use` + 分组 `group.Use`，`c.Next()` 前/后分段执行，支持链后改写响应。 |
| **请求分组** | `Group("/prefix")` 统一前缀 + 组内作用域中间件。 |
| **零成本校验** | 业务参数实现 `Validate() error` 即自动在绑定后、handler 前执行。 |
| **安全错误语义** | 非预期 panic 一律 `500` 且**不回显内部信息**（细节进日志）；显式 `Abort(status, msg)` 才回显。 |
| **可插拔日志** | 内置 `Info/Warn/Error`，可 `SetLogger` 换 zap/slog 等；支持按级别写文件。 |
| **优雅停机** | 收到 `SIGINT/SIGTERM` 或手动 `Shutdown(ctx)`，在宽限期内让在途请求跑完。 |
| **CORS** | 预检、来源/方法/头匹配、凭证、`max-age`（`SetCorsConfig` / `UseCors`）。 |
| **兼容标准库** | `Engine` 实现 `http.Handler`，可直接嵌入 `net/http` 或用 `httptest` 驱动。 |

> 关于性能：热路径已做过预计算绑定计划、对象池复用（ResponseWriter / 路径参数 / Context）、零分配路由匹配等优化。剩余与成熟框架的差距主要来自「任意签名反射调用」这一固有代价——这是本框架核心卖点带来的取舍，非可轻易抹平。

---

## 2. 简单的使用示例

### 最小可运行
```go
package main

import "github.com/gblgo/gbl"

func main() {
    gbl.Get("/hello", func() string {
        return "hello, gbl"
    })

    // 默认监听 :9801；Ctrl+C / kill 触发优雅停机
    gbl.Run()
}
```

### 路径参数 / query / JSON 请求体，全自动绑定
```go
type User struct {
    ID   int
    Name string
}

// :id 按名字绑到参数 id
gbl.Get("/user/:id", func(id string) string {
    return "user " + id
})

// POST + application/json：body 直接反序列化进 User
gbl.Post("/user", func(u User) *User {
    u.Name = strings.ToUpper(u.Name)
    return &u // struct 指针自动以 JSON 返回
})

// query 也按形参名绑定（同名时路径参数优先）。
// GET /user/42?verbose=true -> id="42" 来自路径，verbose=true 来自 query
gbl.Get("/user/:id", func(id string, verbose bool) string {
    return fmt.Sprintf("user %s verbose=%v", id, verbose)
})
```

> **单个参数的取值顺序（从高到低）：** 路径参数（按形参自身名字匹配）→ JSON body → form/query；名字先精确、后忽略大小写。
>
> - 只有一个 struct 参数时，整个 JSON body 一次性解进它。
> - JSON body 且**多个**业务参数时，除路径外的每个参数都必须作为顶层成员、名字与形参一致，例如 `{"verbose": true, "u": {"name": "ada"}}`。
> - form/query 模式下，struct 字段按**字段名**（忽略大小写）从路径与表单值填充，所以 `:id` 也会填进 `u.ID`；但 JSON 模式不会，那里 `u` 只来自 body。

### 直接操作底层类型
```go
import "net/http"

// 想要完全掌控响应，就声明 http.ResponseWriter
gbl.Get("/raw", func(w http.ResponseWriter) {
    w.WriteHeader(http.StatusCreated)
    _, _ = w.Write([]byte("direct"))
})

// 拿原始请求
gbl.Post("/echo", func(r *http.Request) string {
    return r.Header.Get("X-Trace-Id")
})
```

### 中间件 + 分组
```go
// 全局中间件：c.Next() 前=请求进入时，后=响应返回时
gbl.Use(func(c *gbl.Context) {
    c.Set("requestID", newID())
    c.Next()
})

api := gbl.Group("/api")
api.Use(func(c *gbl.Context) {
    if c.Query("token") == "" {
        c.AbortWithStatus(http.StatusUnauthorized) // 中断链，不再进 handler
        return
    }
    c.Next()
})
api.Get("/ping", func() string { return "pong" }) // 实际路径 /api/ping

// 一行接入访问日志中间件
gbl.Use(gbl.AccessLog)
```

### 请求内取值 / 校验 / 中止
```go
type Signup struct {
    Email string
}

// 实现 Validate 即自动校验（handler 不必再判空）
func (s Signup) Validate() error {
    if !strings.Contains(s.Email, "@") {
        return errors.New("bad email") // 只进服务端日志，不回显给客户端
    }
    return nil
}

gbl.Post("/signup", func(c *gbl.Context, s Signup) string {
    rid := gbl.MustValue[string](c, "requestID") // 取中间件写入的值
    return "signed up: " + s.Email + " [" + rid + "]"
})

gbl.Get("/admin", func(c *gbl.Context) {
    if !isAdmin(c) {
        gbl.Abort(http.StatusForbidden, "权限不足") // 显式中止会原样回显
    }
})
```

---

## 3. 对外提供的拓展能力

gbl 把每一个"策略"都做成可替换的扩展点，框架本体只做编排、不硬绑定任何实现。

### 3.1 独立的 Engine 实例
包级 `gbl.Get/Run/...` 是默认引擎的门面；需要**互相隔离**的多引擎（例如每个测试一个）用 `New()`：
```go
e := gbl.New()
e.Group("").Get("/x", func() string { return "y" })
e.Use(myMiddleware)
e.UseCors(corsCfg)
e.ServeHTTP(rec, req) // 实现 http.Handler，可用 httptest 直接驱动，无需起端口
```

### 3.2 Context 请求上下文（中间件/注入通用句柄）
`*Context` 既是中间件的入参，也能作为 handler 参数被注入。常用 API：

| 方法 | 用途 |
|---|---|
| `c.Next()` | 执行链的下一段 |
| `c.Set(k, v)` / `c.Get(k)` / `gbl.Value[T](c,k)` / `gbl.MustValue[T](c,k)` | 请求作用域传值（泛型安全取值） |
| `c.Param(name)` / `c.Query(name)` | 读路径参数 / query |
| `c.JSON(status, v)` / `c.String(status, body)` / `c.Status(code)` | 直接写响应 |
| `c.Result()` / `c.IsWritten()` | 链后读取/判断 handler 结果，做覆写 |
| `c.StatusCode()` / `c.Size()` | 读取有效状态码与响应字节数（供指标/追踪类中间件） |
| `c.Abort()` / `c.AbortWithStatus(s)` / `c.AbortWithStatusJSON(s, v)` | 中断链 |

### 3.3 可插拔 Logger
内置经标准库 `log` 输出 `gbl: LEVEL msg k=v`。想换后端，实现三方法接口并用 `SetLogger` 安装：
```go
type Logger interface {
    Info(msg string, keysAndValues ...any)
    Warn(msg string, keysAndValues ...any)
    Error(msg string, keysAndValues ...any)
}
```
- 只想改目标/前缀，无需自己写实现：`gbl.SetLogger(gbl.NewStdLogger(yourLogger))`。
- **可选实现 `BannerLogger`（`Banner(block string)`）**：拿到的启动横幅会被原样输出，不加时间/级别前缀，避免挤乱 ASCII art。
- **按级别写文件由配置驱动**（见第 4 节），装了自定义 Logger 时内置文件路由会自动让位并给出启动提示，不覆盖你的 sink。

### 3.4 校验约定 `Validatable`
任何被绑定的业务参数（含指针）只要实现 `Validate() error` 就自动校验，不实现的类型零开销：
```go
type Validatable interface { Validate() error }
```

校验是**每个类型写一个 `Validate()` 方法**，而不是每个字段写一个——所有字段的检查都放在这一个方法里。gbl 保持零依赖，不提供基于标签的规则引擎；字段多时用表驱动写法就能平铺展开，也可以在 `Validate()` 里自行调用你的校验库。返回非 nil 即以通用 400 中断请求；错误详情只写服务端日志，不回显给客户端。

```go
type Signup struct {
    Email string
    Age   int
    Name  string
}

// 一个方法覆盖所有字段：加字段只是往表里加一行，不必复制粘贴
func (s Signup) Validate() error {
    checks := []struct {
        field string
        err   error
    }{
        {"email", errIf(s.Email == "", "必填")},
        {"age",   errIf(s.Age < 18, "需满 18 岁")},
        {"name",  errIf(len(s.Name) > 32, "过长")},
    }
    for _, c := range checks {
        if c.err != nil {
            return fmt.Errorf("字段 %q: %w", c.field, c.err) // 只进服务端日志，客户端收 400
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

### 3.5 Panic 渲染钩子 `PanicProcessor`
注册后接管所有被 recover 的值的渲染（含显式中止与真实崩溃）——需自行保证不泄露内部错误：
```go
gbl.RegisterPanicProcessor(func(err any) (any, gbl.HttpStatus) {
    return map[string]any{"error": "server error"}, 500
})
// 或实例级： e.SetPanicProcessor(fn)
```

### 3.6 显式中止 `Abort`
```go
gbl.Abort(status int, msg string) // handler/中间件内调用，status+msg 原样返回客户端
```
区别于裸 `panic`：只有 `Abort` 与框架自身生成的中止会回显，其它崩溃一律 `500` 且细节仅入日志。

### 3.7 观测类中间件（自带 + 自写）
`gbl.AccessLog` 是一枚现成中间件（method/path/status/size/duration）。指标、链路追踪等同样以中间件形式在 `c.Next()` 前后读 `StatusCode()/Size()` 实现——框架不内置任何传输依赖。

### 3.8 CORS
`SetCorsConfig(*CorsConfig)`（默认引擎）或 `UseCors(*CorsConfig)`（实例）安装后，`Run` 自动在链路外层挂上 CORS 处理：预检放行、来源/方法/头匹配、凭证、`max-age`。

> 说明：`CorsConfig` 的字段目前为非导出，对外构造方式仍在完善中；如需在应用中配置 CORS，请留意后续版本补充的构造入口。

---

## 4. 配置参考（application.ini）

框架在启动时读取同目录 `application.ini` 的 `[gbl]` 段；**缺省即全用默认值**。每个键同时接受 `camelCase` 与 `kebab-case` 两种写法。

```ini
[gbl]
name       = myapp
port       = 9801
certFile   = server.crt          ; 与 keyFile 同时给出即启用 TLS
keyFile    = server.key
readTimeout       = 10s
readHeaderTimeout = 5s
writeTimeout      = 10s
idleTimeout       = 60s

; 日志文件（留空=关闭文件日志，仅标准错误输出）
logDir  = ./logs                 ; 自动在该目录生成 info.log/warn.log/error.log
logFile = ./app.log              ; 所有级别合并写一个文件（优先级最低）
; 也可逐级精确指定，优先级：显式级别路径 > logDir/级别.log > logFile
; infoLog  = ./logs/info.log
; warnLog  = ./logs/warn.log
; errorLog = ./logs/error.log
```

| 键 | 默认 | 含义 |
|---|---|---|
| `name` | `gbl` | 名称 |
| `port` | `9801` | 监听端口 |
| `certFile` / `keyFile` | 空 | 二者都非空时启用 HTTPS |
| `readTimeout` / `writeTimeout` / `readHeaderTimeout` / `idleTimeout` | 10s / 10s / 5s / 60s | 服务器超时 |
| `logFile` | 空 | 合并日志文件（最粗粒度落点） |
| `logDir` | 空 | 目录，自动生成 `info.log/warn.log/error.log` |
| `infoLog` / `warnLog` / `errorLog` | 空 | 各级显式路径，优先级最高 |

---

## 5. 安装与运行

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
