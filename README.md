# debug-adapter-go

进程内脚本运行时调试服务。`vm` 为栈式脚本运行时，`debug` 为调试服务：
会话管理、行/条件断点、单步/逐过程步进、调用栈与变量查看、无副作用
表达式求值、异常现场捕获、权限控制与审计。被调试目标与调试服务同进程
运行，不依赖外部服务。

详见 [docs/debugging.md](docs/debugging.md)。

```sh
go test ./... -race -count=1
```
