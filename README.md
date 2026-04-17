# CoderJia TimeKeeper

一个用于缓解 Windows 硬件时钟卡死问题的守护程序。

## 功能

- 启动时通过公网 NTP（大陆常用节点顺序尝试）获取 UTC 基准时间
- 每 1 秒使用单调时钟推演当前时间并调用 `SetSystemTime` 强制覆盖系统 UTC
- 每 10 分钟重新进行一次 NTP 校准
- 同时支持交互模式与原生 Windows Service 模式
- 日志固定写入程序同目录下的 `timekeeper.log`

## 兼容性与 Windows 7

- **Go 1.21 起官方不再支持 Windows 7**。若需在 **Windows 7** 上运行，请使用 **Go 1.20.x**（例如通过 `golang.org/dl/go1.20.14` 安装 `go1.20.14`）在本机编译。
- 本仓库 `go.mod` 已约束为 **Go 1.20**，且 **`golang.org/x/sys` 需为 v0.30.x**（v0.31 起模块要求 Go 1.23，无法在 1.20 下使用）。
- 32 位 Windows 7 请设置 `GOARCH=386` 再编译。

## 构建

```powershell
go mod tidy
go build -o timekeeper.exe .
```

若使用独立安装的 Go 1.20 工具链，将上面命令中的 `go` 换成 `go1.20.14`（或你的 1.20 可执行文件名）。

或直接运行：

```powershell
build.bat
```

## 使用

### 安装服务（管理员）

```powershell
timekeeper.exe -install
```

服务名：`CoderJia-TimeKeeper`（自动启动）

### 卸载服务（管理员）

```powershell
timekeeper.exe -remove
```

### 直接双击运行（无参数）

- 若服务未安装：
  - 管理员权限：自动安装并启动服务
  - 非管理员权限：提示“请以管理员身份运行进行安装”
- 若服务已安装：尝试确保服务处于运行状态

## 常用排查命令

```powershell
sc query CoderJia-TimeKeeper
Get-Content .\timekeeper.log -Tail 100
```
