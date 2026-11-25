# Grand Blue Lore

<p align="center">
  <a href="https://lutme.github.io/gblWeb/#/docs/introduction"><b>官方文档</b></a> ·
  <a href="README.md">English</a> ·
  <a href="README_zh-CN.md">简体中文</a>
</p>

<p align="center">
  <b>gbl</b> 是一个<b>零依赖</b>的 Go Web 框架，完全基于标准库构建。
  核心理念很简单：<b>handler 签名随便写，框架自动装配参数、序列化返回值。</b>
</p>

> **想深入了解？** 本 README 只提供快速概览。完整的中英文指南、边界规则和示例见
> [官方文档](https://lutme.github.io/gblWeb/#/docs/introduction)。建议从
> [快速上手](https://lutme.github.io/gblWeb/#/docs/getting-started) 开始，或查阅
> [API 总览](https://lutme.github.io/gblWeb/#/docs/api-reference)。

---

## 快速开始

需要 **Go 1.27+**。

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

    gbl.Run() // 默认监听 :9801
}
```

```bash
go run .
# GET http://localhost:9801/hello -> hello, gbl
```

## 功能概览

| 领域 | 内置能力 |
|---|---|
| 路由 | 七种 HTTP 动词、静态优先、路径参数、分组、HEAD/OPTIONS 自动兜底 |
| 装配 | 路径、query、表单、JSON、multipart 按类型和名字自动绑定 |
| Handler | 自动文本/JSON 序列化、参数校验、原始响应控制 |
| 中间件 | 洋葱链、`Next`、分组作用域、`Abort` |
| 实时流 | GET SSE、心跳、取消与生命周期回调 |
| 安全 | body/上传限额、CORS、gzip 中间件、启动期快速失败 |
| 运维 | 可插拔日志、TLS、超时、优雅停机 |
| 性能 | 启动期预计算装配计划、对象池、静态路由 map 查找 |

详细指南见
[Handler 与参数装配](https://lutme.github.io/gblWeb/#/docs/handlers)、
[路由](https://lutme.github.io/gblWeb/#/docs/routing)、
[中间件](https://lutme.github.io/gblWeb/#/docs/middleware)、
[SSE](https://lutme.github.io/gblWeb/#/docs/sse) 和
[部署与构建](https://lutme.github.io/gblWeb/#/docs/deployment)。

## Handler 与参数装配

handler 是非 nil 函数，**最多有一个返回值**。不支持变参函数、方法值、方法表达式和泛型实例化；
方法值或方法表达式包一层闭包即可使用。按名字装配的 handler 必须从不同源码行开始;位置歧义会在启动期失败。

### 可注入参数

| 参数 | gbl 注入什么 |
|---|---|
| `*gbl.Context` | 请求上下文与中间件辅助方法 |
| `*http.Request` | 原始请求 |
| `http.ResponseWriter` | 包装后的响应写入器 |
| `gbl.SSEWriter` | GET 路由上的 SSE 流 |
| `multipart.FileHeader` / `[]multipart.FileHeader` | 上传文件，含指针形式 |
| 其他任意类型 | 按参数名或字段名装配的业务值 |

### 装配要点

- 同名**路径参数优先**于所有来源。
- 没命中路径时，由 `Content-Type` 决定来源：非空 JSON 只读 body；其他类型读
  form/query。有 body 的方法中，表单字段覆盖同名 query 值。
- 只有 `POST`、`PUT`、`PATCH` 读取 body，其他方法只使用路径和 query。
- 单个业务参数接收整块 JSON body；多个业务参数读取同名顶层成员。缺失成员使用
  零值，然后交给 `Validatable`（若已实现）校验。
- form/query 名称精确匹配。结构体字段使用 JSON tag；没有 tag 时使用字段名首字母
  小写形式（`Name` → `name`）。依赖此约定时请用 `Id`，不要用 `ID`。

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

### 返回值

| 返回类型 | 响应 |
|---|---|
| `string`、`[]byte`、bool、数字、复数 | 纯文本 |
| 结构体、map、数组、非字节切片、指向复合类型的指针 | 通常为 JSON；`error` 和 nil 按运行时值修正 |
| 接口 | 按运行时值输出纯文本或 JSON；nil 为空 |
| 指向标量的指针 | 纯文本 |
| `error` | 错误消息作为纯文本 |
| nil 指针/接口或无返回值 | 空 body |

有返回值的 handler 会在中间件和业务代码执行前设置临时 `Content-Type`，因此
`Flush` 提交时已有可用类型。响应头尚未提交时，运行时序列化仍可修正最终类型。

指针链、重复值、数组、命名规则和 JSON 边界见完整
[装配指南](https://lutme.github.io/gblWeb/#/docs/handlers)。

## 路由、分组与中间件

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

- 静态路由优先于动态路由；匹配时会归一化连续斜杠和尾部斜杠。重叠的动态模式会在启动期拒绝。
- 未注册的 `HEAD` 复用同路径 `GET`；未注册的 `OPTIONS` 返回带 `Allow` 头的 `204`。
- `c.Next()` 执行剩余链路,返回后的代码在回溯阶段执行。
- `Abort` 和框架的绑定/校验错误都会阻止下游;上游 `Next()` 之后的代码仍会执行。
- `GroupUse` 必须在该分组的路由注册之前调用。

## SSE 与原始响应

GET handler 声明 `gbl.SSEWriter` 即可推送事件。SSE handler 不能有返回值；使用
`Send` 发送，并在 `Done` 关闭时退出。

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

框架自动管理 SSE 响应头、Flush、空闲心跳、写超时和取消逻辑。可选
`OnError`、`OnComplete` 回调用于处理流失败与终止;回调在流终止且在途写排空后异步执行。

需要直接控制 HTTP 响应时，注入 `http.ResponseWriter`。标准可选接口和
`http.ResponseController` 均可使用。直接 `Write` 或 `Hijack` 后，框架不再自动写回
返回值；Hijack 后的连接由应用负责，成功停机时框架会关闭已跟踪的连接。

回调、1xx/101、写超时、Hijack 与 WebSocket 集成见
[SSE 指南](https://lutme.github.io/gblWeb/#/docs/sse) 和
[ResponseWriter 指南](https://lutme.github.io/gblWeb/#/docs/response-writer)。

## 校验与日志

业务参数实现 `Validate() error` 后，会在装配完成、handler 执行前自动校验。校验
失败返回 `400`，响应体为你的错误文本。nil 指针直接传给 handler，不调用 `Validate`。

```go
func (u User) Validate() error {
    if u.Name == "" {
        return errors.New("name is required")
    }
    return nil
}
```

`Logger` 是框架内部事件与 `Info`、`Warn`、`Error` 的统一日志契约。默认实现向私有
控制台 logger 输出带日期的文本，仅在真实终端上为级别着色；在 `Run` 之前调用
`SetLogger` 即可接入自己的日志后端。

## 限额、配置与停机

| 设置 | 默认值 |
|---|---|
| 端口 | `9801` |
| 请求 body 总大小 | 5 MiB |
| multipart 内存阈值 | 2 MiB |
| 单个上传文件 | 1 MiB |
| 信号停机窗口 | 5 秒 |

`SetMaxFileSize` 只是内存/磁盘溢写阈值，不是上传上限；`SetMaxBodySize` 和
`SetMaxUploadFileSize` 才是硬限制。

```go
gbl.Configure(func(c *gbl.Config) {
    c.Port = "8080"
    c.WriteTimeout = 30 * time.Second
})

gbl.Get("/ready", func() string { return "ok" })
gbl.Run()
```

所有注册与配置 API 都必须在 **`Run` 之前**调用。`Run` 自动处理
`SIGINT`/`SIGTERM`；需要主动停机时调用 `Shutdown(ctx)`。Shutdown 会先取消 SSE
流，再等待在途请求，成功后会关闭已跟踪的 Hijack 连接。

## 构建与部署

普通生产构建不需要额外工具：

```bash
go build -trimpath -o app ./cmd/api
```

按名字装配需要从源码或二进制 DWARF 恢复参数名。除非所有相关 handler 都使用可
自描述的结构体参数，否则请避免 `-ldflags "-w"` 和 `"-s"`。详见
[部署与构建](https://lutme.github.io/gblWeb/#/docs/deployment)。

## License

[MIT](./LICENSE) © lutme
