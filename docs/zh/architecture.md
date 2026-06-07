# dae 项目架构文档

## 1. 项目概述

dae（发音同 "die"）是一个基于 eBPF 的高性能 Linux 透明代理解决方案。它在内核层面通过 TC（Traffic Control）和 cgroup 程序拦截网络流量，结合用户态的路由引擎和多种代理协议实现，提供零拷贝、低开销的透明代理能力。

### 核心特性

- **内核级流量拦截**：通过 eBPF TC classifier 和 cgroup 程序在内核态完成流量捕获与路由决策，避免了传统 TPROXY 方案的额外系统开销。
- **多协议支持**：通过 `github.com/daeuniverse/outbound` 库支持 Shadowsocks、VMess、Trojan、TUIC、Hysteria2 等主流代理协议，以及 TLS、WebSocket、gRPC 等传输层。
- **域名级路由**：支持基于域名（精确匹配、后缀匹配、关键字匹配、正则表达式）的路由规则，配合 Aho-Corasick 自动机和简洁 Trie 实现高性能域名匹配。
- **DNS 路由与劫持**：内置完整的 DNS 路由引擎，支持请求/响应级别的 DNS 路由策略，可劫持系统 DNS 请求并通过 DoH/DoT/DoQ 等加密 DNS 协议转发。
- **热重载**：支持通过 SIGUSR1 信号触发配置热重载，实现零停机时间的分代切换（generation handoff），包括连接排水、DNS 控制器复用和回退保护。
- **故障转移**：支持多种拨号器选择策略（随机、固定、最小延迟、故障转移），故障转移策略具有三态状态机（主用、备用、恢复中）和指数退避探测。
- **网络命名空间隔离**：使用 Netkit（内核 6.7+）或 veth 设备创建专用网络命名空间，实现 eBPF 重定向的对等（peer）优化。

### 运行环境要求

- Linux 内核 5.17+（完整功能需 6.6+ 以支持 Netkit 和 TCX）
- 支持 eBPF 的内核配置
- cgroup v2

---

## 2. 目录结构概览

```
dae/
├── main.go                    # 程序入口，注册模糊 JSON 解码器，调用 cmd.Execute()
├── cmd/                       # CLI 命令定义（cobra 框架）
│   ├── cmd.go                 # 根命令，版本信息，Execute() 入口
│   ├── run.go                 # 核心运行子命令，信号处理，热重载编排
│   ├── reload.go              # reload 子命令，发送 SIGUSR1
│   ├── reload_manager.go      # 热重载管理器，分代切换，DNS 手柄
│   ├── suspend.go             # suspend 子命令，发送 SIGUSR2
│   ├── export.go              # export 子命令，导出配置大纲
│   ├── validate.go            # validate 子命令，校验配置文件
│   ├── sysdump.go             # sysdump 子命令，收集系统诊断信息
│   ├── trace.go               # trace 子命令（需 build tag），eBPF 包追踪
│   ├── completion.go          # shell 自动补全生成
│   ├── runner.go              # Runner 结构体，封装运行时状态
│   ├── internal/
│   │   └── su.go              # 权限提升工具（sudo/doas/run0/pkexec）
│   ├── dae-ebpf-audit/        # eBPF 校验审计工具
│   └── generators/
│       └── gen_ebpf_sync/     # eBPF 常量同步代码生成器
├── common/                    # 通用工具库
│   ├── utils.go               # 网络地址、反射、字节序转换等工具函数
│   ├── debug.go               # 运行时内存诊断
│   ├── assets/                # 资源文件定位（geoip/geosite）
│   ├── bitlist/               # 紧凑位列表，用于 eBPF 路由规则编码
│   ├── consts/                # 全局常量与类型定义（拨号模式、协议类型、eBPF 特性门控等）
│   ├── errors/                # 标准化错误分类与检测
│   ├── json/                  # 模糊布尔值 JSON 解码器
│   ├── netutils/              # DNS 解析、双栈解析、UDP 读写工具
│   └── subscription/          # 代理订阅解析（HTTP/文件/SIP008）
├── component/                 # 可复用组件层
│   ├── interface_manager.go   # 网络接口事件监听与回调分发
│   ├── daedns/                # dae DNS 路由引擎（客户端 + 路由器）
│   ├── dns/                   # DNS 路由核心（请求/响应匹配、上游管理）
│   ├── outbound/              # 出站管理
│   │   ├── outbound.go        # 协议注册（空白导入）
│   │   ├── dialer_group.go    # 拨号器组管理与选择策略
│   │   ├── dialer_selection_policy.go  # 选择策略配置解析
│   │   ├── failover_controller.go      # 故障转移状态机
│   │   ├── filter.go          # 拨号器过滤与注解
│   │   └── dialer/            # 拨号器核心实现
│   │       ├── dialer.go               # Dialer 结构体，健康检查集合
│   │       ├── alive_dialer_set.go     # 存活拨号器集合与延迟选择
│   │       ├── connectivity_check.go   # 周期性健康检查（TCP HTTP + UDP DNS）
│   │       ├── latency_probe.go        # 延迟探测（不修改健康状态）
│   │       ├── recovery_state.go       # 指数退避恢复检测
│   │       ├── annotation.go           # 注解解析（add_latency, priority）
│   │       ├── register.go             # 从订阅链接创建拨号器
│   │       ├── sockopt.go              # 底层套接字选项控制
│   │       ├── sticky_cache.go         # 代理 IP 粘性缓存
│   │       └── transport_cache_scope.go # 传输缓存命名空间隔离
│   ├── routing/               # 路由引擎
│   │   ├── ir.go              # 路由中间表示（NormalizedProgram）
│   │   ├── normalize.go       # 规范化与后端 lowering
│   │   ├── optimizer.go       # 路由规则优化器（别名、合并、去重、geodata 展开）
│   │   ├── matcher_builder.go # 匹配器构建器
│   │   ├── function_parser.go # 路由函数参数解析器工厂
│   │   └── domain_matcher/    # 域名匹配后端
│   │       ├── ahocorasick_slimtrie.go  # 生产级：Aho-Corasick + 简洁 Trie
│   │       ├── bruteforce.go            # 暴力匹配（测试用）
│   │       └── go_regexp_nfa.go         # Go 正则 NFA 匹配
│   └── sniffing/              # 协议嗅探
│       ├── sniffer.go         # 嗅探引擎（流/包模式）
│       ├── conn_sniffer.go    # TCP 连接嗅探包装器
│       ├── tls.go             # TLS ClientHello SNI 提取
│       ├── http.go            # HTTP Host 头提取
│       ├── quic.go            # QUIC Initial 包解密与 SNI 提取
│       └── internal/
│           └── quicutils/     # QUIC 解密工具（HKDF、AES-GCM、帧重组）
├── config/                    # 配置系统
│   ├── config.go              # 配置数据模型与构造函数
│   ├── config_merger.go       # include 指令递归合并
│   ├── decode.go              # 分节解码器注册与分发
│   ├── parser.go              # 反射式分节到结构体解析器
│   ├── marshal.go             # 配置序列化回 .dae 格式
│   ├── outline.go             # 配置大纲 JSON 导出（供工具链使用）
│   ├── patch.go               # 后解析补丁（校验、默认值、规范化）
│   ├── desc.go                # 人类可读的字段描述
│   └── bootstrap_resolver.go  # 引导 DNS 解析器地址
├── control/                   # 控制面与数据面核心
│   ├── control.go             # go:generate bpf2go 指令
│   ├── control_plane.go       # ControlPlane 主结构体与构造函数
│   ├── control_plane_core.go  # eBPF TC/cgroup 挂载，网络命名空间管理
│   ├── control_plane_drain.go # 连接排水追踪
│   ├── generation_state.go    # 分代状态（出站、路由匹配器）
│   ├── dial.go                # 代理拨号逻辑
│   ├── tcp.go                 # TCP 连接处理（嗅探 + 路由 + 中继）
│   ├── udp.go                 # UDP 包处理（DNS/QUIC 嗅探 + 路由）
│   ├── dns.go                 # DNS 转发器实现（UDP/TCP/DoT/DoH/DoQ）
│   ├── dns_control.go         # DNS 控制器（缓存、上游选择、去重）
│   ├── dns_cache.go           # DNS 响应缓存（预打包、BPF 差分更新）
│   ├── dns_listener.go        # 本地 DNS 监听器
│   ├── routing_matcher_builder.go  # 路由规则编译到 eBPF map 和用户态匹配器
│   ├── routing_matcher_userspace.go # 用户态路由匹配器（回退路径）
│   ├── domain_routing_tracker.go    # 域名路由位图追踪与 BPF map 增量更新
│   ├── connectivity.go        # 出站连通性状态写入 eBPF map
│   ├── bpf_utils.go           # eBPF 工具函数、map 操作、程序加载
│   ├── bpf_stub.go            # 非 eBPF 构建的桩类型定义
│   ├── bpf_purge.go           # 启动时清除残留 TC 过滤器
│   ├── datapath_janitor.go    # 过期 BPF map 条目定期清理
│   ├── netns_utils.go         # 网络命名空间创建与配置
│   ├── anyfrom_pool.go        # UDP GSO 连接池
│   ├── udp_endpoint_pool.go   # UDP 端点池管理
│   ├── udp_flow.go            # UDP 流标识与会话管理
│   ├── tcp_copy_linux.go      # Linux splice(2) 零拷贝 TCP 中继
│   ├── runtime_stats.go       # 流量统计与速率计算
│   ├── kern/
│   │   ├── tproxy.c           # 核心 eBPF C 程序（3471 行，18 个入口点）
│   │   └── ebpf_sync_defs.h   # 自动生成的 C/Go 共享常量头文件
│   └── kern/tests/            # eBPF 内核级单元测试
├── pkg/                       # 通用库（纯叶子包，无内部依赖）
│   ├── anybuffer/             # 泛型无符号整数缓冲区
│   ├── config_parser/         # ANTLR4 配置文件解析器
│   ├── ebpf_internal/         # eBPF 辅助工具（内核版本检测、ELF 解析）
│   ├── geodata/               # V2Ray 格式 GeoIP/GeoSite 数据库解码
│   ├── logger/                # logrus 日志配置
│   └── trie/                  # 简洁 Trie（rank/select 位图）
├── trace/                     # eBPF 包追踪工具（build tag: trace）
│   ├── trace.go               # kprobe 挂载与事件处理
│   ├── kallsyms.go            # /proc/kallsyms 符号表解析
│   └── kern/trace.c           # 追踪 eBPF C 程序
├── hack/                      # CI、维护、mock、模板工具
├── scripts/                   # checkpatch.pl、内存分析、eBPF 审计
├── install/                   # systemd 服务文件、安装脚本、空配置模板
├── Dockerfile                 # 多阶段 Docker 构建
├── Makefile                   # 构建系统（eBPF 编译 + Go 构建）
└── example.dae                # 示例配置文件
```

---

## 3. 核心模块详解

### 3.1 `cmd` -- 命令行入口

CLI 入口基于 `github.com/spf13/cobra` 框架。根命令定义在 `cmd.go`，通过 `Execute()` 启动。主要子命令：

| 子命令 | 文件 | 功能 |
|--------|------|------|
| `run` | `run.go` | 启动代理守护进程，是系统核心运行循环 |
| `reload` | `reload.go` | 向运行中的进程发送 SIGUSR1 触发热重载 |
| `suspend` | `suspend.go` | 发送 SIGUSR2 暂停代理（清空所有组） |
| `export` | `export.go` | 导出配置结构 JSON 大纲 |
| `validate` | `validate.go` | 校验配置文件语法与语义 |
| `sysdump` | `sysdump.go` | 收集路由表、接口、sysctl、iptables 等诊断信息打包为 tar.gz |
| `trace` | `trace.go` | eBPF 包追踪（需 `trace` build tag） |
| `completion` | `completion.go` | 生成 bash/zsh/fish 自动补全脚本 |

**`run.go`** 是最复杂的文件，负责：
- 读取并深拷贝配置，创建 `ControlPlane`
- 注册信号处理：SIGUSR1（热重载）、SIGUSR2（暂停）、SIGINT/SIGTERM（关闭）
- 编排分代切换（generation handoff）：新控制面就绪后原子切换，旧控制面排水后销毁
- DNS 控制器复用决策：通过配置指纹（`dnsConfigFingerprint`）判断 DNS 配置是否变化
- PID 文件管理和 sdnotify 就绪通知

**`reload_manager.go`** 实现热重载管理器：
- 合并并发的重载请求（coalescing）
- 准备 DNS 手柄（prepared handoff hooks）
- 管理旧代控制面的优雅退役

**`cmd/internal/su.go`** 提供权限自动提升，依次尝试 `sudo`、`doas`、`run0`、`pkexec`。

### 3.2 `common` -- 通用工具层

被几乎所有其他模块依赖的底层工具库。

**`common/utils.go`** 提供：
- IPv6 字节序转换（`Ipv6ByteSliceToUint32Array` 等）用于 eBPF 互操作
- 反射式配置解析辅助（`SetValueHierarchicalStruct`、`FuzzyDecode`）
- MAC 地址解析、端口范围解析
- SO_MARK 解析（`ResolveSoMarkFromDae`）
- Magic network 字符串编码（`MagicNetwork`、`MagicNetworkWithIPVersion`）

**`common/consts/`** 是项目中被导入最频繁的包（93 处引用），定义了：
- 拨号模式（`DialMode_Ip`、`DialMode_Domain` 等）
- 拨号器选择策略（`Random`、`Fixed`、`MinAverage10Latencies`、`Failover` 等）
- L4 协议类型、IP 版本类型、匹配类型、出站索引等 eBPF 共享常量
- 路由函数名（`domain`、`ip`、`sip`、`port`、`sport`、`l4proto`、`pname` 等）
- eBPF 特性版本门控（`BasicFeatureVersion`、`BpfTimerFeatureVersion`、`TcxFeatureVersion`、`NetkitFeatureVersion` 等）

**`common/errors/`** 提供热路径优化的错误分类函数，区分可忽略的网络错误（连接关闭、超时、断管）和真正的故障。包含 BPF 错误包装，为用户生成可操作的错误消息。

**`common/netutils/`** 实现通过任意 netproxy 拨号器的 DNS 解析（支持 UDP/TCP DNS 传输）、系统 DNS 配置解析、并发 IPv4+IPv6 双栈解析（可选竞速）。

**`common/subscription/`** 解析代理订阅源：HTTP/HTTPS URL（支持文件持久化离线回退）、本地 `file://` 路径、Base64 编码节点列表、SIP008 JSON 格式。

### 3.3 `component` -- 可复用组件层

#### 3.3.1 `component/outbound/dialer` -- 拨号器核心

**`Dialer`** 结构体是整个出站系统的核心类型，封装了 `netproxy.Dialer` 并扩展了：
- 8 个健康检查集合（TCP/UDP x IPv4/IPv6 x DNS/Data）
- 存活状态追踪与恢复检测（指数退避）
- HTTP 客户端缓存
- 代理失败追踪
- 热重载健康快照继承

**`AliveDialerSet`** 维护线程安全的存活拨号器集合，支持基于延迟的选择（最小延迟、带排除的随机选择），并支持策略热切换。

**健康检查系统**（`connectivity_check.go`）：
- TCP 检查：HTTP GET/CONNECT 请求到配置的检查 URL
- UDP 检查：DNS 查询到配置的检查域名
- 全局蚁群式工作池（基于 `panjf2000/ants`）
- 冷启动抖动（避免同时检查）
- 连续失败阈值触发不可用标记

**恢复状态机**（`recovery_state.go`）：
- 三个健康域：TCP、DNS-UDP、Data-UDP
- 指数退避恢复检测
- 稳定性计数确认

#### 3.3.2 `component/outbound` -- 出站管理

**`DialerGroup`** 管理一组拨号器，支持多种选择策略：
- `Random`：随机选择
- `Fixed`：固定索引
- `MinAverage10Latencies`：最近 10 次延迟平均值最小
- `MinMovingAverageLatencies`：移动平均延迟最小
- `MinLastLatency`：最近一次延迟最小
- `Failover`：基于优先级的故障转移

**`FailoverController`** 实现三态故障转移状态机：
- `PrimaryActive`：主拨号器活跃
- `FallbackActive`：备用拨号器活跃
- `Recovering`：恢复探测中（指数退避）

**`DialerSet`** 从订阅链接创建拨号器，支持过滤表达式（按名称正则/关键字、订阅标签）和注解（`add_latency` 延迟偏移、`priority` 优先级）。

#### 3.3.3 `component/dns` -- DNS 路由引擎

**`Dns`** 结构体是 DNS 路由核心：
- 解析上游定义（支持 7 种 scheme：UDP、TCP、TCP+UDP、TLS、QUIC、HTTPS、H3）
- 构建请求匹配器和响应匹配器
- 请求路由：基于 qname 域名模式和 qtype 值选择上游
- 响应路由：基于 qname、qtype、响应 IP 和来源上游决定接受/拒绝/转发

**请求路由分类**（`request_rule_split.go`）将规则分为四类：
- DNS 规则（qname/qtype 匹配）
- Sub 规则（订阅选择器）
- Node 规则（节点选择器）
- SubNode 规则（子节点选择器）

#### 3.3.4 `component/daedns` -- dae DNS 客户端与路由器

**`Router`** 构建 DNS 路由引擎，根据订阅/节点元数据（标签、链接、名称匹配）选择 DNS 上游解析器。支持正则/关键字匹配，封装拨号器的 DNS 感知解析行为。

**`LookupIPAddr`** 通过多传输协议（UDP、TCP、TLS、HTTPS、H3、QUIC）解析主机名，支持并发查找去重和连接取消钩子。

#### 3.3.5 `component/routing` -- 路由引擎

**中间表示**（`NormalizedProgram`）：路由规则的规范化 IR，经过优化器处理后由匹配器后端消费。

**优化器管道**：
1. `AliasOptimizer`：别名重写
2. `MergeAndSortRulesOptimizer`：合并与排序
3. `DeduplicateParamsOptimizer`：参数去重
4. `DatReaderOptimizer`：geosite/geoip DAT 文件展开（带缓存）

**域名匹配后端**：
- `AhocorasickSlimtrie`（生产级）：Aho-Corasick 自动机（关键字匹配）+ 简洁 Trie（后缀/全匹配）+ Go regexp（正则匹配），并行构建
- `Bruteforce`：O(n) 暴力匹配，用于测试
- `GoRegexpNfa`：将所有模式编译为单个 Go 正则 NFA

#### 3.3.6 `component/sniffing` -- 协议嗅探

从流或数据包中提取域名信息：
- **TLS**：解析 ClientHello 记录，提取 SNI 扩展
- **HTTP**：从原始请求字节中提取 Host 头
- **QUIC**：解密 QUIC Initial 包，重组 CRYPTO 帧，提取 TLS ClientHello SNI

`ConnSniffer` 包装 `net.Conn`，透明地嗅探前几个字节后中继剩余数据。实现 `io.WriterTo` 和 `io.ReaderFrom` 以支持 splice(2) 零拷贝中继。

QUIC 解密链（`quicutils`）：
1. 根据版本确定 Initial Salt
2. HKDF-Expand-Label 派生密钥
3. AES 头保护移除
4. AES-GCM 载荷解密
5. CRYPTO 帧重组与 TLS ClientHello 解析

### 3.4 `config` -- 配置系统

基于 ANTLR4 语法的自定义 `.dae` 配置格式，使用大括号分节：

```
global {
    tproxy_port: 12345
    lan_interface: eth0
    dial_mode: domain
}
group(proxy) {
    filter: subtag(proxy)
    policy: min_moving_average
}
routing {
    domain(suffix:google.com) -> proxy
    ip(geoip:cn) -> direct
}
```

**解析流水线**：
1. `Merger` 递归处理 `include` 指令（支持 glob 模式），检测循环引用
2. ANTLR4 解析器将文本转为 AST（`Section` -> `Item` -> `Param`/`RoutingRule`/`Section`）
3. 反射式 `SectionParser` 使用 `mapstructure` 标签将 AST 映射到 Go 结构体
4. 后解析补丁链：校验引导 DNS、规范化 HTTP 方法、填充默认 DNS 回退、转换 `must_` 前缀出站名

**配置数据模型**：

```go
type Config struct {
    Global       Global           // 全局设置（tproxy 端口、接口、拨号模式等）
    Subscription []KeyableString  // 订阅源（tag:url 或 url）
    Node         []KeyableString  // 节点定义
    Group        []Group          // 节点组（过滤器、策略、健康检查覆盖）
    Routing      Routing          // 路由规则与回退
    Dns          Dns              // DNS 配置（上游、请求/响应路由）
}
```

### 3.5 `control` -- 控制面与数据面核心

这是项目中最庞大、最核心的包，约 40 个 Go 源文件，桥接内核态 eBPF 程序与用户态代理/拨号器逻辑。

**`ControlPlane`**（`control_plane.go`）是核心结构体，协调：
- eBPF 程序加载与 map 管理
- 网络命名空间配置
- 路由规则编译
- DNS 控制器初始化
- TCP/UDP 监听器管理

**`controlPlaneCore`**（`control_plane_core.go`）是"代核心"，持有 BPF 对象，负责 TC 过滤器和 cgroup 程序的挂载/卸载，管理 Netkit/veth 设备状态。

**TCP 处理流水线**（`tcp.go`）：
1. 接受 eBPF 拦截的 TCP 连接
2. 域名嗅探（HTTP Host / TLS SNI）
3. 通过路由匹配器确定出站
4. 选择拨号器并建立代理连接
5. 双向流量中继（splice 零拷贝 / gather-write / 缓冲拷贝）

**UDP 处理流水线**（`udp.go`）：
1. 接收 eBPF 拦截的 UDP 包
2. DNS 嗅探 / QUIC SNI 嗅探
3. 路由决策
4. 出站拨号
5. 回复包发送（支持 GSO 批量发送）

**DNS 控制器**（`dns_control.go`）：
- 管理 DNS 缓存（预打包二进制格式，BPF map 差分更新）
- 上游选择与 singleflight 去重
- 并发查询限制
- 缓存淘汰回调
- RFC 8767 陈旧数据服务（stale-while-revalidate）
- RFC 8305 Happy Eyeballs 解析延迟（偏好等待）

**DNS 转发器**（`dns.go`）实现多传输协议：
- UDP / TCP 基础转发
- DoT（DNS-over-TLS）：TLS 连接池 + 流水线
- DoH（DNS-over-HTTPS）：HTTP/2 + HTTP/3
- DoQ（DNS-over-QUIC）：QUIC 连接池

**路由编译**（`routing_matcher_builder.go`）：
- 将用户配置的路由规则编译为 `bpfMatchSet` 结构体写入 `routing_map`
- 构建 LPM Trie 写入 `lpm_array_map`
- 同时构建用户态 `RoutingMatcher` 作为回退路径

**连接排水**（`control_plane_drain.go`）：引用计数模式追踪活跃 TCP 连接和 UDP 端点，暴露空闲通道 `IdleCh()` 供重载协调器等待。

### 3.6 `pkg` -- 纯库层

不依赖任何内部包的叶子库。

**`pkg/config_parser`**：基于 ANTLR4 的 `.dae` 配置文件解析器。将原始文本解析为 `Section` -> `Item` -> `Param`/`RoutingRule`/`Function` AST。

**`pkg/ebpf_internal`**：从 cilium/ebpf v0.10.0 内部代码 fork 的低级 eBPF 辅助工具。提供内核版本检测（通过 vDSO）、安全 ELF 解析、原始套接字创建。嵌套的 `internal/unix` 子包在非 Linux 平台提供桩实现。

**`pkg/trie`**：简洁 Trie（prefix tree），使用 rank/select 位图实现紧凑存储（比朴素 Trie 节省约 40% 内存），用于 CIDR/域名前缀匹配。

**`pkg/geodata`**：V2Ray 格式 GeoIP/GeoSite `.dat` 数据库的流式解码器。提供 protobuf 消息类型用于 IP/CIDR 路由规则和域名匹配规则。

**`pkg/anybuffer`**：泛型无符号整数可增长缓冲区，用于 eBPF 路由规则编码。

**`pkg/logger`**：配置 logrus 日志器，支持前缀文本格式化和通过 lumberjack 的日志轮转。

### 3.7 `trace` -- eBPF 包追踪

通过 build tag `trace` 条件编译。使用 kprobe 挂载到内核函数，追踪 `sk_buff` 生命周期。支持按 IP 版本、L4 协议、端口过滤，以及仅捕获丢包事件。

`ReadKallsyms()` 解析 `/proc/kallsyms` 构建内核符号表，`NearestSymbol()` 通过二分查找将地址解析为函数名。

---

## 4. 模块依赖关系

### 4.1 依赖矩阵

```
                   common  config  control  component  cmd  pkg  trace
common              [自]     Y       -        -        -    Y     -
config               Y      [自]     -        -        -    Y     -
control              Y       Y      [自]      Y        -    Y     -
component            Y       Y       -       [自]      -    Y     -
cmd                  Y       Y       Y         Y      [自]  Y    Y
trace                Y       -       -         -        -   Y    [自]
pkg                  -       -       -         -        -  [自]   -
```

### 4.2 依赖图

```
                        ┌─────────┐
                        │  main   │
                        └────┬────┘
                             │
                        ┌────▼────┐
                        │   cmd   │ ─────────────────────────┐
                        └────┬────┘                          │
                  ┌──────────┼──────────┐                    │
                  │          │          │                    │
             ┌────▼────┐ ┌───▼───┐ ┌────▼────┐        ┌─────▼─────┐
             │ control  │ │config │ │component│        │   trace   │
             └────┬────┘ └───┬───┘ └────┬────┘        └─────┬─────┘
                  │          │          │                    │
         ┌────────┼──────┐   │    ┌─────┼──────┐            │
         │        │      │   │    │     │      │            │
    ┌────▼───┐ ┌──▼──┐ ┌─▼───┐ │ ┌─────▼┐ ┌───▼────┐      │
    │component│ │config│ │common│ │ │dns   │ │outbound│      │
    │(子包)   │ │      │ │     │ │ │      │ │/dialer │      │
    └────────┘ └──────┘ └──┬──┘ │ └──────┘ └────────┘      │
                           │    │                           │
                      ┌────▼────▼────┐                      │
                      │     pkg     │ ◄─────────────────────┘
                      │(config_parser│
                      │ ebpf_internal│
                      │ trie geodata)│
                      └─────────────┘
```

### 4.3 关键依赖路径

- `cmd` 是顶层入口，依赖 `control`（核心运行时）、`config`（配置解析）、`component`（DNS/拨号器）、`trace`（追踪工具）
- `control` 是最重的消费者，导入几乎所有其他顶层包（`common`、`component`、`config`、`pkg`）
- `common/consts` 被 93 处引用，是全局共享常量层
- `pkg/config_parser` 被 38 处引用，是配置解析的基础设施
- `pkg` 是纯叶子包，不依赖任何内部包
- `common` 通过子包 `common/consts` 间接依赖 `pkg/ebpf_internal`；`config` 依赖 `common`。两者通过子包粒度避免了循环依赖

### 4.4 被依赖排名（Top 10）

| 排名 | 包 | 引用次数 | 角色 |
|------|-----|---------|------|
| 1 | `common/consts` | 93 | 全局常量与类型定义 |
| 2 | `pkg/config_parser` | 38 | 配置解析基础设施 |
| 3 | `common` | 33 | 通用工具函数 |
| 4 | `component/outbound/dialer` | 30 | 拨号器核心 |
| 5 | `config` | 28 | 配置数据模型 |
| 6 | `component/outbound` | 16 | 出站管理 |
| 7 | `component/dns` | 15 | DNS 路由 |
| 8 | `common/netutils` | 13 | 网络工具 |
| 9 | `common/errors` | 12 | 错误分类 |
| 10 | `component/routing` | 12 | 路由引擎 |

---

## 5. 关键数据流

### 5.1 TCP 连接处理流水线

```
应用程序发起 TCP 连接
        │
        ▼
┌──────────────────────────────────────────────────────┐
│ eBPF 内核态 (tproxy.c)                               │
│                                                      │
│  1. cgroup/connect4/6 记录 socket cookie → PID 映射  │
│  2. TC wan_ingress 拦截出站 SYN 包                   │
│  3. 查 routing_map 进行规则匹配                       │
│  4. 查 domain_routing_map 域名路由（如有缓存）         │
│  5. 查 outbound_connectivity_map 出站存活状态          │
│  6. 写 routing_handoff_map 传递路由结果                │
│  7. 设置 skb->mark = TPROXY_MARK                     │
│  8. bpf_redirect_peer() 重定向到 dae0peer 接口        │
└──────────────────────────────────────────────────────┘
        │
        ▼
┌──────────────────────────────────────────────────────┐
│ dae0peer 接口 (tproxy_dae0peer_ingress)               │
│                                                      │
│  1. 从 routing_handoff_map 读取路由结果                │
│  2. bpf_sk_assign() 将 skb 绑定到控制面监听 socket    │
└──────────────────────────────────────────────────────┘
        │
        ▼
┌──────────────────────────────────────────────────────┐
│ 用户态 TCP 处理 (control/tcp.go)                      │
│                                                      │
│  1. Accept() 接受连接                                  │
│  2. 从 routing_handoff_map 读取路由决策                │
│  3. 域名嗅探（TLS SNI / HTTP Host）                   │
│  4. 用户态路由匹配（回退路径）                         │
│  5. 根据路由结果选择出站组                             │
│  6. DialerGroup.Select() 选择拨号器                    │
│  7. 建立代理连接                                      │
│  8. 双向流量中继（splice 零拷贝优先）                  │
│  9. 更新 domain_routing_map（域名 → 路由位图）         │
└──────────────────────────────────────────────────────┘
```

### 5.2 UDP 包处理流水线

```
应用程序发送 UDP 包
        │
        ▼
┌──────────────────────────────────────────────────────┐
│ eBPF 内核态                                          │
│                                                      │
│  1. cgroup/sendmsg4/6 记录 socket cookie → PID       │
│  2. TC wan_ingress 拦截 UDP 包                        │
│  3. 路由匹配（同 TCP）                                │
│  4. 写 udp_conn_state_map 记录连接状态                 │
│  5. 重定向到 dae0peer                                 │
└──────────────────────────────────────────────────────┘
        │
        ▼
┌──────────────────────────────────────────────────────┐
│ 用户态 UDP 处理 (control/udp.go)                      │
│                                                      │
│  1. 批量读取 UDP 包（ipv4/ipv6 ReadBatch）            │
│  2. DNS 嗅探 / QUIC SNI 嗅探                         │
│  3. 路由决策（与 TCP 共享匹配器）                      │
│  4. 出站拨号（复用或新建端点）                         │
│  5. 发送代理请求                                      │
│  6. 接收响应并回复客户端（sendPkt / sendPktViaListener）│
└──────────────────────────────────────────────────────┘
```

### 5.3 DNS 请求处理流水线

```
系统解析器发起 DNS 查询
        │
        ▼
┌──────────────────────────────────────────────────────┐
│ DNS 监听器 (control/dns_listener.go)                  │
│  监听 tcp+udp://127.0.0.1:53                          │
└──────────────────────────────────────────────────────┘
        │
        ▼
┌──────────────────────────────────────────────────────┐
│ DNS 控制器 (control/dns_control.go)                   │
│                                                      │
│  1. singleflight 去重                                 │
│  2. 查询 DNS 缓存（预打包响应，直接返回）              │
│  3. 请求路由匹配 → 选择上游解析器                      │
│  4. 转发到上游（UDP/TCP/DoT/DoH/DoQ）                 │
│  5. 响应路由匹配 → 接受/拒绝/转发                      │
│  6. RFC 8767 陈旧数据服务（背景异步刷新）               │
│  7. RFC 8305 Happy Eyeballs 偏好等待                   │
│  8. 缓存响应，差分更新 domain_routing_map              │
│  9. 返回响应给客户端                                   │
└──────────────────────────────────────────────────────┘
```

### 5.4 热重载数据流

```
用户执行 dae reload
        │
        ▼ 发送 SIGUSR1
┌──────────────────────────────────────────────────────┐
│ run.go: 信号处理循环                                   │
│                                                      │
│  1. 读取新配置                                        │
│  2. 深拷贝配置                                        │
│  3. 创建新 ControlPlane                               │
│     a. 加载 eBPF 程序（新代核心）                      │
│     b. 编译路由规则                                   │
│     c. 初始化 DNS 控制器                               │
│        - 计算 DNS 配置指纹                             │
│        - 如果指纹相同，复用旧 DNS 控制器               │
│     d. 继承拨号器健康快照（避免重新探测）               │
│     e. 挂载新 TC 过滤器（flip 机制零停机切换）         │
│  4. 等待新控制面就绪                                   │
│  5. 原子切换：旧控制面开始排水                         │
│  6. 等待旧控制面空闲（所有连接完成）                   │
│  7. 销毁旧控制面                                      │
│  8. 写入重载完成信号文件                               │
└──────────────────────────────────────────────────────┘
```

---

## 6. 配置系统

### 6.1 文件格式

dae 使用自定义的 `.dae` 配置格式，基于 ANTLR4 语法定义（`github.com/daeuniverse/dae-config-dist`）。格式特点：

- 大括号分节（`section_name { ... }`）
- 键值对（`key: value`）
- 路由规则（`function(param) -> outbound`）
- AND 组合（`domain(suffix:google.com) && l4proto(tcp) -> proxy`）
- include 指令（`include: config.d/*.dae`）

### 6.2 配置段说明

**global**（必需）：

| 字段 | 说明 |
|------|------|
| `tproxy_port` | 透明代理监听端口 |
| `lan_interface` | LAN 接口列表 |
| `wan_interface` | WAN 接口列表 |
| `dial_mode` | 拨号模式：ip / domain / domain+ / domain++ |
| `tcp_check_url` | TCP 健康检查 URL |
| `udp_check_dns` | UDP 健康检查 DNS |
| `log_level` | 日志级别 |
| `so_mark_from_dae` | dae 自身流量的 SO_MARK |

**group**：

| 字段 | 说明 |
|------|------|
| `filter` | 节点过滤表达式 |
| `policy` | 选择策略：random / fixed / min / min_avg10 / min_moving_avg / failover |
| `tcp_check_url` | 覆盖全局的 TCP 检查 URL |
| `failover_recovery` | 故障转移恢复配置（探测间隔、退避参数） |

**dns**：

| 字段 | 说明 |
|------|------|
| `upstream` | DNS 上游服务器列表 |
| `routing.request` | 请求路由规则 |
| `routing.response` | 响应路由规则 |
| `fixed_domain_ttl` | 固定域名 TTL |
| `ipversion_prefer` | IP 版本偏好（4/6） |
| `optimistic_cache` | 启用 RFC 8767 陈旧数据服务 |

**routing**：

| 字段 | 说明 |
|------|------|
| 规则行 | `function(params) -> outbound` |
| fallback | 回退出站 |

### 6.3 解析流水线详解

```
.dae 文件
    │
    ▼
┌─────────────────────────────┐
│ config_merger.go             │
│ Merger.Merge()               │
│ - 递归处理 include 指令       │
│ - glob 模式展开              │
│ - 循环引用检测               │
│ - 文件权限校验 (0640/0600)   │
│ 输出: []*config_parser.Section│
└─────────────┬───────────────┘
              │
              ▼
┌─────────────────────────────┐
│ pkg/config_parser            │
│ Parse()                      │
│ - ANTLR4 lexer/parser        │
│ - 构建 AST                   │
│ 输出: Section → Item[]       │
│       Item = Param |         │
│              RoutingRule |   │
│              Section         │
└─────────────┬───────────────┘
              │
              ▼
┌─────────────────────────────┐
│ config/decode.go             │
│ decodeConfigSection()        │
│ - 按节名分发到对应解码器      │
│ - 6 个解码器:                │
│   global, subscription,      │
│   node, group, routing, dns  │
└─────────────┬───────────────┘
              │
              ▼
┌─────────────────────────────┐
│ config/parser.go             │
│ SectionParser()              │
│ - 反射式映射到 Go 结构体     │
│ - mapstructure 标签驱动      │
│ - 支持默认值、必需字段       │
│ - FunctionOrString 联合类型  │
└─────────────┬───────────────┘
              │
              ▼
┌─────────────────────────────┐
│ config/patch.go              │
│ 后解析补丁链:                │
│ 1. patchBootstrapResolver    │
│ 2. patchTcpCheckHttpMethod   │
│ 3. patchEmptyDns             │
│ 4. patchMustOutbound         │
└─────────────┬───────────────┘
              │
              ▼
        config.Config 结构体
```

### 6.4 配置大纲导出

`config/outline.go` 通过反射自省 `Config` 结构体，生成 JSON 格式的配置大纲（包含字段名、类型、默认值、是否必需、描述），供 IDE 插件和 UI 工具使用。`cmd export outline` 子命令调用此功能。

---

## 7. eBPF 架构

### 7.1 程序概览

dae 的 eBPF 程序位于 `control/kern/tproxy.c`（3471 行），包含 18 个程序入口点，分为三类：

**TC 分类器**（Traffic Control）：

| 程序 | 挂载点 | 功能 |
|------|--------|------|
| `tproxy_lan_ingress_l2/l3` | LAN 接口 ingress | 拦截 LAN 入站流量，路由决策 |
| `tproxy_lan_egress_l2/l3` | LAN 接口 egress | 处理 LAN 出站流量（局域网内流量） |
| `tproxy_wan_ingress_l2/l3` | WAN 接口 ingress | 拦截 WAN 入站流量（回程流量） |
| `tproxy_wan_egress_l2/l3` | WAN 接口 egress | 拦截 WAN 出站流量（主要拦截点） |
| `tproxy_dae0peer_ingress` | dae0peer 接口 ingress | 处理重定向到 dae0peer 的包 |
| `tproxy_dae0_ingress` | dae0 接口 ingress | 处理 dae0 的回复流量 |

`_l2` 变体处理以太网帧封装接口（`link_h_len=14`），`_l3` 变体处理原始 IP 接口（隧道、tun 设备，`link_h_len=0`）。

**Cgroup 程序**：

| 程序 | 功能 |
|------|------|
| `tproxy_wan_cg_sock_create` | 记录 socket cookie → PID/进程名映射 |
| `tproxy_wan_cg_sock_release` | socket 关闭时清理映射 |
| `tproxy_wan_cg_connect4/6` | 跟踪 IPv4/IPv6 connect() 调用 |
| `tproxy_wan_cg_sendmsg4/6` | 跟踪 IPv4/IPv6 UDP sendmsg() 调用 |

**其他**：
- `tproxy_sockops`：占位（无操作，保留 ABI）
- `tproxy_sk_msg_redir`：已禁用（返回 SK_PASS），因 `bpf_redirect_hash()` 导致内核 panic 而停用

### 7.2 eBPF Map

| Map | 类型 | 用途 |
|-----|------|------|
| `routing_map` | ARRAY | 路由规则数组（最多 1024 条 `match_set`） |
| `routing_meta_map` | ARRAY | 活跃路由规则数量 |
| `lpm_array_map` | ARRAY_OF_MAPS | LPM Trie 数组，用于 IP 前缀匹配 |
| `domain_routing_map` | HASH | 域名 → 路由位图缓存（128 字节值） |
| `outbound_connectivity_map` | ARRAY | 每出站存活/死亡状态（256 出站 x 3 域 x 2 IP 版本） |
| `cookie_pid_map` | HASH | Socket cookie → PID/进程名映射 |
| `listen_socket_map` | SOCKMAP | TCP4/UDP/TCP6 监听 socket（用于 sk_msg 重定向） |
| `redirect_track` | HASH | 重定向条目追踪（IP 对 → ifindex + MAC） |
| `tcp_conn_state_map` | HASH | TCP 连接状态与路由缓存 |
| `udp_conn_state_map` | HASH | UDP 连接状态与路由元数据 |
| `routing_handoff_map` | HASH | 首包路由决策在 TC hook 间传递 |
| `wan_egress_scratch_map` | PERCPU_ARRAY | WAN egress 临时空间（避免栈溢出） |

### 7.3 编译与加载流程

```
                    ┌──────────────────────────────┐
                    │ ebpf_sync_spec.json           │
                    │ (共享常量规范)                 │
                    └──────────────┬───────────────┘
                                   │ gen_ebpf_sync
                    ┌──────────────▼───────────────┐
                    │ ebpf_sync_defs.h (C 头文件)   │
                    │ ebpf_generated.go (Go 常量)   │
                    └──────────────┬───────────────┘
                                   │
        ┌──────────────────────────▼──────────────────────────┐
        │ tproxy.c (包含 ebpf_sync_defs.h)                    │
        │ + bpf2go (cilium/ebpf/cmd/bpf2go)                   │
        │ + clang 编译为 BPF ELF 字节码                        │
        └──────────────────────────┬──────────────────────────┘
                                   │
                    ┌──────────────▼───────────────┐
                    │ bpf_bpfel.go                  │
                    │ (Go 嵌入的 eBPF 对象)          │
                    │ bpfObjects 结构体              │
                    └──────────────┬───────────────┘
                                   │
                    ┌──────────────▼───────────────┐
                    │ bpf_utils.go                  │
                    │ fullLoadBpfObjects()          │
                    │ 1. loadBpf() → CollectionSpec │
                    │ 2. 重写 PARAM 变量            │
                    │ 3. 定制 map spec              │
                    │ 4. LoadAndAssign() 加载到内核  │
                    └──────────────┬───────────────┘
                                   │
                    ┌──────────────▼───────────────┐
                    │ control_plane_core.go         │
                    │ 挂载:                          │
                    │ - clsact qdisc + TC 过滤器     │
                    │ - cgroup 程序到 cgroupv2 root  │
                    │ - Netkit/veth 设备对           │
                    └──────────────────────────────┘
```

### 7.4 eBPF 常量同步

为确保 C 和 Go 代码共享相同的枚举/常量定义，项目使用代码生成机制：

1. `common/consts/ebpf_sync_spec.json` 定义共享常量规范
2. `cmd/generators/gen_ebpf_sync/main.go` 读取规范，同时生成：
   - `common/consts/ebpf_generated.go`（Go 常量）
   - `control/kern/ebpf_sync_defs.h`（C 宏定义）
3. CI 中通过 `make ebpf-sync-check` 验证生成文件与规范一致

### 7.5 TC 过滤器零停机切换

热重载时使用 `flip` 机制（0 或 1）实现 TC 过滤器的零停机切换：

- 旧控制面使用 `minor = flip` 的 handle
- 新控制面使用 `minor = 1 - flip` 的 handle
- `FilterAdd` 新过滤器后 `FilterDel` 旧过滤器
- 内核保证在切换期间不会丢失包

### 7.6 路由匹配流程（内核态）

```
收到数据包
    │
    ▼
提取 5 元组（src/dst IP、src/dst port、L4 proto）
    │
    ▼
提取元数据（MAC、进程名、DSCP）
    │
    ▼
遍历 routing_map 中的 match_set 条目
    │
    ├─ 域名匹配 → 查 domain_routing_map（缓存命中则直接返回位图）
    │              缓存未命中 → 返回 "需要用户态处理"
    │
    ├─ IP 匹配 → 查 lpm_array_map 中的 LPM Trie
    │
    ├─ 端口匹配 → 直接比较
    │
    ├─ L4 协议匹配 → 直接比较
    │
    ├─ 进程名匹配 → 查 cookie_pid_map
    │
    └─ MAC 匹配 → 直接比较
    │
    ▼
匹配成功 → 查 outbound_connectivity_map 确认出站存活
    │
    ▼
设置路由结果 → bpf_redirect_peer() 到 dae0peer
    │
    ▼
写 routing_handoff_map → 用户态读取路由决策
```

### 7.7 网络命名空间架构

```
┌─────────────────────────────────────────────────┐
│ 主命名空间                                        │
│                                                  │
│  WAN 接口 ◄── TC wan_egress/ingress ──► eBPF    │
│  LAN 接口 ◄── TC lan_egress/ingress  ──► eBPF    │
│                                                  │
│  ┌─────────────────────────────────┐             │
│  │ dae0 (Netkit L3 / veth)        │             │
│  └──────────────┬──────────────────┘             │
│                 │ peer                             │
│  ┌──────────────▼──────────────────┐             │
│  │ dae0peer                        │             │
│  │ TC dae0peer_ingress             │             │
│  │ → bpf_sk_assign() 到监听 socket  │             │
│  └─────────────────────────────────┘             │
│                                                  │
│  控制面监听器 (tproxy_port)                       │
│  接受被 sk_assign 绑定的连接                       │
└─────────────────────────────────────────────────┘
```

Netkit 设备（内核 6.7+）提供 `bpf_redirect_peer()` 优化，允许跨命名空间重定向时绕过路由查找和安全钩子，直接将包送达目标命名空间的网络栈。

---

## 8. 构建与部署

### 8.1 构建系统

项目使用 Makefile 驱动构建，主要目标：

| 目标 | 功能 |
|------|------|
| `make dae` | 完整构建：eBPF 编译 + Go 构建 |
| `make ebpf` | 仅编译 eBPF 程序 |
| `make ebpf-sync` | 生成 eBPF 共享常量 |
| `make ebpf-sync-check` | CI 验证生成文件一致性 |
| `make ebpf-test` | 运行 eBPF 内核级单元测试 |
| `make ebpf-audit` | eBPF 校验器审计 |
| `make ebpf-lint` | eBPF C 代码风格检查（checkpatch.pl） |
| `make fmt` | Go 代码格式化 |
| `make submodule` | 初始化 git 子模块 |

**构建依赖**：
- Go 1.26+
- clang/llvm（编译 eBPF C 程序）
- git（子模块、版本号）

**构建约束**：
- 目标平台：Linux（`//go:build linux`）
- CGO 默认禁用（`CGO_ENABLED=0`）
- 交叉编译通过 `GOARCH` 控制
- 版本号从 git 历史自动生成：`unstable-YYYYMMDD.rCOUNT.HASH`

**Build tags**：
- `trace`：启用 eBPF 包追踪功能
- `dae_stub_ebpf`：桩构建（非 eBPF 平台）
- `dae_bpf_tests`：启用 eBPF 内核级测试

### 8.2 Go 构建参数

```bash
go build \
  -trimpath \
  -ldflags "-s -w \
    -X github.com/daeuniverse/dae/cmd.Version=$VERSION \
    -X github.com/daeuniverse/dae/common/consts.MaxMatchSetLen_=1024" \
  -tags=$(BUILD_TAGS) \
  -o dae \
  .
```

`GOEXPERIMENT` 默认启用 `heapminimum512kib` 和 `randomizedheapbase64` 以优化内存管理。

### 8.3 Docker 构建

多阶段构建：

1. **builder 阶段**：基于 `golang:1.26-bookworm`，安装 llvm-15/clang-15，执行完整构建
2. **运行阶段**：基于 `alpine`，仅复制二进制文件和 geoip/geosite 数据库

```dockerfile
ENTRYPOINT ["dae", "run", "-c", "/etc/dae/config.dae"]
```

配置文件权限要求 0600（`chmod 0600 /etc/dae/config.dae`）。

### 8.4 systemd 部署

`install/dae.service` 提供 systemd 服务文件。运行时文件：
- PID 文件：`/var/run/dae.pid`
- 信号进度文件：`/var/run/dae.signal_progress`
- 中止文件：`/var/run/dae.abort`
- 资源文件目录：`/usr/local/share/dae/` 或 `/usr/share/dae/`

### 8.5 环境变量

| 变量 | 说明 |
|------|------|
| `DAE_LOCATION_ASSET` | 自定义资源文件搜索路径 |

### 8.6 特性版本门控

eBPF 功能根据内核版本逐步启用：

| 特性 | 最低内核版本 |
|------|-------------|
| 基础功能 | 5.17 |
| bpf_timer | 5.17 |
| TCX | 6.6 |
| Netkit | 6.7 |
| bpf_get_current_task | 5.17 |
| bpf_redirect_peer | 5.10 |

运行时通过 `pkg/ebpf_internal.KernelVersion()` 检测内核版本，动态决定可用特性。
