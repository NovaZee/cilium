# Cilium CNI IPAM 工作流程详解

本文档详细介绍 Cilium CNI 中 IPAM（IP Address Management）的工作流程，特别是当 `ipam=crd` 时，Pod IP 的分配逻辑。

---

## 目录

1. [IPAM 模式概述](#1-ipam-模式概述)
2. [整体架构](#2-整体架构)
3. [ipam=crd 工作流程](#3-ipamcrd-工作流程)
4. [CiliumNode CRD 数据结构](#4-ciliumnode-crd-数据结构)
5. [与 Cilium-Agent 的通信](#5-与-cilium-agent-的通信)
6. [多池 IPAM 模式](#6-多池-ipam-模式)
7. [IP 释放机制](#7-ip-释放机制)
8. [关键代码位置](#8-关键代码位置)

---

## 1. IPAM 模式概述

### 1.1 支持的 IPAM 模式

**文件位置**: `pkg/ipam/option/option.go`

```go
const (
    IPAMKubernetes       = "kubernetes"        // Kubernetes 原生 PodCIDR
    IPAMCRD             = "crd"               // CRD 模式（本文重点）
    IPAMENI             = "eni"               // AWS ENI IPAM
    IPAMAzure           = "azure"             // Azure IPAM
    IPAMClusterPool     = "cluster-pool"      // 集群池模式
    IPAMMultiPool       = "multi-pool"        // 多池模式
    IPAMAlibabaCloud    = "alibabacloud"      // 阿里云 ENI
    IPAMDelegatedPlugin = "delegated-plugin"  // CNI 委托插件
)
```

### 1.2 IPAM 模式选择

在 `plugins/cilium-cni/cmd/cmd.go` 的 Add 方法中：

```go
if conf.IpamMode == ipamOption.IPAMDelegatedPlugin {
    // 使用委托插件（如 host-local、dhcp 等）
    ipam, releaseIPsFunc, err = allocateIPsWithDelegatedPlugin(...)
} else {
    // 使用 Cilium Agent（包括 CRD、Kubernetes、ENI 等模式）
    ipam, releaseIPsFunc, err = allocateIPsWithCiliumAgent(scopedLogger, c, cniArgs, epConf.IPAMPool())
}
```

---

## 2. 整体架构

### 2.1 组件关系图

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         Cilium IPAM 架构                                      │
└─────────────────────────────────────────────────────────────────────────────┘

    ┌──────────────┐
    │   Kubelet    │
    │  (CNI ADD)   │
    └──────┬───────┘
           │
           ▼
    ┌──────────────────────────────────────────────────────────────────┐
    │                    cilium-cni Plugin                             │
    │  ┌────────────────────────────────────────────────────────────┐  │
    │  │ plugins/cilium-cni/cmd/cmd.go::Cmd.Add()                   │  │
    │  └────────────────────────────────────────────────────────────┘  │
    │                              │                                    │
    │                              ▼                                    │
    │  ┌────────────────────────────────────────────────────────────┐  │
    │  │ allocateIPsWithCiliumAgent()                               │  │
    │  │ - 构建参数                                                 │  │
    │  │ - 调用 client.IPAMAllocate()                               │  │
    │  └────────────────────────────────────────────────────────────┘  │
    └──────────────────────────────┬───────────────────────────────────┘
                                   │
                                   ▼ HTTP REST API
    ┌──────────────────────────────────────────────────────────────────┐
    │                    Cilium Agent (Daemon)                        │
    │  ┌────────────────────────────────────────────────────────────┐  │
    │  │ pkg/ipam/api/ipam_api_handler.go                           │  │
    │  │ IpamPostIpamHandler.Handle()                               │  │
    │  └────────────────────────────────────────────────────────────┘  │
    │                              │                                    │
    │                              ▼                                    │
    │  ┌────────────────────────────────────────────────────────────┐  │
    │  │ pkg/ipam/ipam.go::IPAM.AllocateNextWithExpiration()        │  │
    │  └────────────────────────────────────────────────────────────┘  │
    │                              │                                    │
    │                              ▼                                    │
    │  ┌────────────────────────────────────────────────────────────┐  │
    │  │ pkg/ipam/allocator.go::IPAM.AllocateNextFamily()           │  │
    │  └────────────────────────────────────────────────────────────┘  │
    │                              │                                    │
    │                              ▼                                    │
    │  ┌────────────────────────────────────────────────────────────┐  │
    │  │ pkg/ipam/crd.go::crdAllocator.AllocateNext()               │  │
    │  │ (CRD 模式分配器)                                           │  │
    │  └────────────────────────────────────────────────────────────┘  │
    │                              │                                    │
    │                              ▼                                    │
    │  ┌────────────────────────────────────────────────────────────┐  │
    │  │ nodeStore.allocateNext()                                   │  │
    │  │ - 从 CiliumNode.Spec.IPAM.Pool 读取可用 IP                 │  │
    │  │ - 返回 IP 和 AllocationIP                                  │  │
    │  └────────────────────────────────────────────────────────────┘  │
    └──────────────────────────────┬───────────────────────────────────┘
                                   │
                                   ▼
    ┌──────────────────────────────────────────────────────────────────┐
    │                    Kubernetes API Server                        │
    │  ┌────────────────────────────────────────────────────────────┐  │
    │  │ CiliumNode CRD                                             │  │
    │  │ - Spec.IPAM.Pool: 可用 IP 池                               │  │
    │  │ - Status.IPAM.Used: 已使用 IP                              │  │
    │  └────────────────────────────────────────────────────────────┘  │
    └──────────────────────────────────────────────────────────────────┘
                                   ▲
                                   │ Watch/Update
    ┌──────────────────────────────┴───────────────────────────────────┐
    │                    Cilium Operator                              │
    │  - 为节点填充 Spec.IPAM.Pool                                     │
    │  - 管理 IP 释放握手                                              │
    └──────────────────────────────────────────────────────────────────┘
```

### 2.2 核心组件

| 组件 | 文件位置 | 功能 |
|------|----------|------|
| **CNI 插件** | `plugins/cilium-cni/cmd/cmd.go` | 接收 Kubelet CNI 调用，与 Agent 通信 |
| **HTTP 客户端** | `pkg/client/ipam.go` | 封装与 Agent 的 REST API 通信 |
| **API Handler** | `pkg/ipam/api/ipam_api_handler.go` | 处理 IPAM API 请求 |
| **IPAM 核心** | `pkg/ipam/ipam.go` | IPAM 分配主逻辑 |
| **CRD 分配器** | `pkg/ipam/crd.go` | CRD 模式的具体分配实现 |
| **多池管理器** | `pkg/ipam/metadata/manager.go` | 管理 PodIPPool CRD |

---

## 3. ipam=crd 工作流程

### 3.1 完整调用链路

```
Kubelet CNI ADD 调用
    │
    │ [plugins/cilium-cni/cmd/cmd.go:523]
    ▼ Cmd.Add()
    │
    ├─ 解析 CNI 配置 (types.LoadNetConf)
    │
    ├─ 检查 IPAM 模式
    │
    ▼ [Line 172]
 allocateIPsWithCiliumAgent()
    │
    ├─ 构建 Pod 标识 (namespace/podname)
    │
    │ [pkg/client/ipam.go:18]
    ▼ client.IPAMAllocate()
    │
    ├─ 构建 HTTP 请求参数
    │  - family: "ipv4" / "ipv6" / ""
    │  - owner: "namespace/podname"
    │  - pool: IP 池名称
    │  - expiration: 是否设置过期时间
    │
    ▼ POST /ipam (HTTP REST API)
    │
    │ [pkg/ipam/api/ipam_api_handler.go:39]
    ▼ IpamPostIpamHandler.Handle()
    │
    ├─ 解析请求参数
    │
    │ [pkg/ipam/ipam.go:243]
    ▼ IPAM.AllocateNextWithExpiration()
    │
    ├─ 选择地址族 (IPv4/IPv6)
    │
    │ [pkg/ipam/allocator.go:194]
    ▼ IPAM.AllocateNextFamily()
    │
    ├─ 根据模式选择分配器
    │
    │ [pkg/ipam/crd.go:966]
    ▼ crdAllocator.AllocateNext()
    │
    ├─ 加锁保护并发
    │
    │ [pkg/ipam/crd.go:635]
    ▼ nodeStore.allocateNext()
    │
    ├─ 遍历 CiliumNode.Spec.IPAM.Pool
    ├─ 查找未分配的 IP
    │  - allocated[ip] 不存在
    │  - ipInfo.Owner == ""
    │  - 不在释放握手中
    │
    ▼ 返回 IP 和 AllocationIP
    │
    ├─ 构建分配结果 (包含网关、CIDR)
    │
    ├─ 标记为已分配 (allocated[ip] = owner)
    │
    ├─ 触发 CiliumNode CRD 更新
    │
    ▼ 返回 IPAMResponse
    │
    ▼ HTTP Response
    │
    ▼ cilium-cni 接收响应
    │
    ├─ 提取 IP 地址
    ├─ 配置网络接口
    │
    ▼ 返回给 Kubelet
```

### 3.2 详细代码说明

#### 步骤 1: CNI ADD 入口

**文件**: `plugins/cilium-cni/cmd/cmd.go:523`

```go
func (cmd *Cmd) Add(args *skel.CmdArgs) error {
    // 1. 解析 CNI 配置
    n, err := types.LoadNetConf(args.StdinData)
    if err != nil {
        return fmt.Errorf("failed to load netconf: %w", err)
    }

    // 2. 解析 CNI 参数
    cniArgs := types.LoadCNIArgs(args.Args)

    // 3. 创建 REST API 客户端
    c := client.NewClient(cmd.determineAPIEndpoints(n))

    // 4. 根据 IPAM 模式选择分配方式
    var ipam *models.IPAMResponse
    var releaseIPsFunc func(context.Context)

    if conf.IpamMode == ipamOption.IPAMDelegatedPlugin {
        // 委托插件模式
        ipam, releaseIPsFunc, err = allocateIPsWithDelegatedPlugin(...)
    } else {
        // Cilium Agent 模式 (包括 CRD)
        ipam, releaseIPsFunc, err = allocateIPsWithCiliumAgent(
            scopedLogger,
            c,
            cniArgs,
            epConf.IPAMPool(),
        )
    }

    // 5. 配置网络接口
    ...
}
```

#### 步骤 2: 调用 Cilium Agent

**文件**: `plugins/cilium-cni/cmd/cmd.go:172`

```go
func allocateIPsWithCiliumAgent(
    logger *slog.Logger,
    client *client.Client,
    cniArgs *types.ArgsSpec,
    ipamPoolName string,
) (*models.IPAMResponse, func(context.Context), error) {
    // 构建 Pod 标识符
    podName := string(cniArgs.K8S_POD_NAMESPACE) + "/" + string(cniArgs.K8S_POD_NAME)

    // 调用 cilium-agent API
    ipam, err := client.IPAMAllocate("", podName, ipamPoolName, true)
    if err != nil {
        return nil, nil, fmt.Errorf("failed to allocate IP via cilium-agent: %w", err)
    }

    // 构建释放函数
    releaseFunc := func(ctx context.Context) {
        if ipam.Address != nil {
            // 释放 IPv4
            if ipam.Address.IPV4 != nil {
                releaseIP(logger, client, ipam.Address.IPV4, ipam.Address.IPV4PoolName)
            }
            // 释放 IPv6
            if ipam.Address.IPV6 != nil {
                releaseIP(logger, client, ipam.Address.IPV6, ipam.Address.IPV6PoolName)
            }
        }
    }

    return ipam, releaseFunc, nil
}
```

#### 步骤 3: HTTP API 请求

**文件**: `pkg/client/ipam.go:18`

```go
func (c *Client) IPAMAllocate(family, owner, pool string, expiration bool) (*models.IPAMResponse, error) {
    params := ipam.NewPostIpamParams().WithTimeout(api.ClientTimeout)

    // 设置请求参数
    if family != "" {
        params.SetFamily(&family)
    }
    if owner != "" {
        params.SetOwner(&owner)
    }
    if pool != "" {
        params.SetPool(&pool)
    }
    params.SetExpiration(&expiration)

    // 发送 POST /ipam 请求
    resp, err := c.Ipam.PostIpam(params)
    if err != nil {
        return nil, Hint(err)
    }

    return resp.Payload, nil
}
```

#### 步骤 4: Agent 处理请求

**文件**: `pkg/ipam/api/ipam_api_handler.go:39`

```go
func (r *IpamPostIpamHandler) Handle(params ipamapi.PostIpamParams) middleware.Responder {
    family := strings.ToLower(swag.StringValue(params.Family))
    owner := swag.StringValue(params.Owner)
    pool := ipam.Pool(swag.StringValue(params.Pool))
    expiration := swag.BoolValue(params.Expiration)

    // 调用 IPAM 核心分配逻辑
    ipv4Result, ipv6Result, err := r.IPAM.AllocateNextWithExpiration(
        family,
        owner,
        pool,
        r.DefaultExpirationTimeout,
    )
    if err != nil {
        return ipamapi.NewPostIpamDefault(http.StatusInternalServerError).WithPayload(err)
    }

    // 构建响应
    resp := &models.IPAMResponse{
        HostAddressing: node.GetNodeAddressing(r.Logger),
        Address:        &models.AddressPair{},
    }

    if ipv4Result != nil {
        resp.Address.IPV4 = ipv4Result.IP.IP.String()
        resp.Address.IPV4PoolName = string(ipv4Result.Pool)
        resp.Gateway = ipv4Result.Gateway.String()
    }

    if ipv6Result != nil {
        resp.Address.IPV6 = ipv6Result.IP.IP.String()
        resp.Address.IPV6PoolName = string(ipv6Result.Pool)
        if ipv6Result.Gateway != nil {
            resp.IPV6Gateway = ipv6Result.Gateway.String()
        }
    }

    return ipamapi.NewPostIpamCreated().WithPayload(resp)
}
```

#### 步骤 5: IPAM 核心分配

**文件**: `pkg/ipam/ipam.go:243`

```go
func (ipam *IPAM) AllocateNextWithExpiration(family, owner string, pool Pool, expiration time.Duration) (ipv4Result, ipv6Result *AllocationResult, err error) {
    // 根据 family 选择分配哪些地址族
    if (family == "ipv6" || family == "") && ipam.ipv6Allocator != nil {
        ipv6Result, err = ipam.ipv6Allocator.AllocateNext(owner, pool)
    }

    if (family == "ipv4" || family == "") && ipam.ipv4Allocator != nil {
        ipv4Result, err = ipam.ipv4Allocator.AllocateNext(owner, pool)
    }

    return ipv4Result, ipv6Result, err
}
```

#### 步骤 6: CRD 分配器

**文件**: `pkg/ipam/crd.go:966`

```go
func (a *crdAllocator) AllocateNext(owner string, pool Pool) (*AllocationResult, error) {
    a.mutex.Lock()
    defer a.mutex.Unlock()

    // 从 nodeStore 获取下一个可用 IP
    ip, ipInfo, err := a.store.allocateNext(a.allocated, a.family, owner)
    if err != nil {
        return nil, fmt.Errorf("unable to allocate IP: %w", err)
    }

    // 构建分配结果
    result, err := a.buildAllocationResult(ip, ipInfo)
    if err != nil {
        return nil, fmt.Errorf("unable to build allocation result: %w", err)
    }

    // 标记为已分配
    a.markAllocated(ip, owner, *ipInfo)

    // 触发 CiliumNode CRD 更新
    a.store.refreshTrigger.TriggerWithReason(fmt.Sprintf("allocation of IP %s", ip.String()))

    return result, nil
}
```

#### 步骤 7: 从 CiliumNode 读取 IP

**文件**: `pkg/ipam/crd.go:635`

```go
func (n *nodeStore) allocateNext(allocated ipamTypes.AllocationMap, family ipamTypes.Family, owner string) (net.IP, *ipamTypes.AllocationIP, error) {
    n.mutex.RLock()
    defer n.mutex.RUnlock()

    if n.ownNode == nil {
        return nil, nil, fmt.Errorf("CiliumNode for own node is not available")
    }

    // 遍历 CiliumNode.Spec.IPAM.Pool 中的所有 IP
    for ip, ipInfo := range n.ownNode.Spec.IPAM.Pool {
        // 检查是否已被分配
        if _, ok := allocated[ip]; ok {
            continue
        }

        // 检查是否在释放握手中
        if n.isIPInReleaseHandshake(ip) {
            continue
        }

        // 检查是否有所有者
        if ipInfo.Owner != "" {
            continue
        }

        // 检查地址族是否匹配
        parsedIP := net.ParseIP(ip)
        if ipamTypes.DeriveFamily(parsedIP) != family {
            continue
        }

        // 找到可用 IP
        return parsedIP, &ipInfo, nil
    }

    return nil, nil, errors.New("no IPs currently available on the node")
}
```

---

## 4. CiliumNode CRD 数据结构

### 4.1 CRD 结构定义

**文件位置**: `pkg/ipam/types/types.go:124`

```go
// IPAMSpec 定义节点的 IPAM 规范（嵌入在 CiliumNode 中）
type IPAMSpec struct {
    // Pool 是可供此节点分配的 IPv4 地址列表
    // 格式: map[string]AllocationIP
    // key: IP 地址字符串, value: IP 元数据
    Pool AllocationMap `json:"pool,omitempty"`

    // IPv6Pool 是可供此节点分配的 IPv6 地址列表
    IPv6Pool AllocationMap `json:"ipv6-pool,omitempty"`

    // Pools 包含分配给此节点的 IPAM 池引用
    Pools IPAMPoolSpec `json:"pools,omitempty"`

    // PodCIDRs 是分配给此节点的 Kubernetes Pod CIDR
    PodCIDRs []string `json:"podCIDRs,omitempty"`

    // MinAllocate 是节点首次启动时必须分配的最小 IP 数量
    MinAllocate int `json:"min-allocate,omitempty"`

    // MaxAllocate 是可以分配给节点的最大 IP 数量
    MaxAllocate int `json:"max-allocate,omitempty"`

    // PreAllocate 定义必须可用于分配的 IP 地址缓冲区数量
    PreAllocate int `json:"pre-allocate,omitempty"`

    // MaxAboveWatermark 是超过 PreAllocate 水位线分配的最大地址数
    MaxAboveWatermark int `json:"max-above-watermark,omitempty"`
}

// IPAMStatus 定义节点的 IPAM 状态
type IPAMStatus struct {
    // Used 列出所有已分配且正在使用的 IPv4 地址
    Used AllocationMap `json:"used,omitempty"`

    // IPv6Used 列出所有已分配且正在使用的 IPv6 地址
    IPv6Used AllocationMap `json:"ipv6-used,omitempty"`

    // ReleaseIPs 跟踪每个被视为释放候选项的 IPv4 地址的状态
    ReleaseIPs map[string]IPReleaseStatus `json:"release-ips,omitempty"`

    // IPv6ReleaseIPs 跟踪每个被视为释放候选项的 IPv6 地址的状态
    IPv6ReleaseIPs map[string]IPReleaseStatus `json:"ipv6-release-ips,omitempty"`

    // OverflowPool 是否启用了溢出池
    OverflowPool bool `json:"overflow-pool,omitempty"`
}

// AllocationIP 表示 IP 的元数据
type AllocationIP struct {
    // Owner 是 IP 的所有者（通常是 Pod 名称：namespace/podname）
    Owner string `json:"owner,omitempty"`

    // Resource 表示 IP 关联的资源（例如 AWS ENI ID）
    Resource string `json:"resource,omitempty"`
}

// AllocationMap 是 IP 到 AllocationIP 的映射
type AllocationMap map[string]AllocationIP
```

### 4.2 CiliumNode CRD 示例

```yaml
apiVersion: cilium.io/v2
kind: CiliumNode
metadata:
  name: node-1
spec:
  ipam:
    pool:
      "10.0.0.1": {}
      "10.0.0.2": {}
      "10.0.0.3": {}
      "10.0.0.4": {}
    ipv6-pool:
      "fd00::1": {}
      "fd00::2": {}
      "fd00::3": {}
    max-allocate: 100
    min-allocate: 10
    pre-allocate: 50
status:
  ipam:
    used:
      "10.0.0.1":
        owner: "default/pod-1"
      "10.0.0.2":
        owner: "default/pod-2"
    ipv6-used:
      "fd00::1":
        owner: "default/pod-1"
    release-ips:
      "10.0.0.3": "mark-for-release"
    ipv6-release-ips:
      "fd00::2": "ready-for-release"
```

### 4.3 IP 释放状态

**文件位置**: `pkg/ipam/option/option.go`

```go
const (
    // IPAMMarkForRelease 表示 IP 已被标记为释放候选项
    IPAMMarkForRelease IPReleaseStatus = "mark-for-release"

    // IPAMDoNotRelease 表示 IP 不应被释放（仍在使用）
    IPAMDoNotRelease IPReleaseStatus = "do-not-release"

    // IPAMReadyForRelease 表示 IP 已准备好被释放
    IPAMReadyForRelease IPReleaseStatus = "ready-for-release"
)
```

---

## 5. 与 Cilium-Agent 的通信

### 5.1 通信协议

| 属性 | 值 |
|------|-----|
| **协议** | HTTP/1.1 REST API |
| **传输** | Unix Domain Socket |
| **序列化** | JSON |
| **客户端** | Go-Swagger 生成的类型安全客户端 |
| **服务端** | Cilium Agent REST API Server |
| **超时** | `api.ClientTimeout` (默认 30s) |

### 5.2 API 端点

| 方法 | 路径 | 功能 | 参数 |
|------|------|------|------|
| **POST** | `/ipam` | 分配下一个可用 IP | `family`, `owner`, `pool`, `expiration` |
| **POST** | `/ipam/{ip}` | 分配指定 IP | `ip`, `family`, `owner`, `pool` |
| **DELETE** | `/ipam/{ip}` | 释放 IP | `ip`, `pool` |

### 5.3 请求示例

**分配 IP 请求**:

```http
POST /ipam HTTP/1.1
Host: cilium-agent
Content-Type: application/json

{
  "family": "ipv4",
  "owner": "default/my-pod",
  "pool": "",
  "expiration": true
}
```

**成功响应**:

```http
HTTP/1.1 201 Created
Content-Type: application/json

{
  "address": {
    "ipv4": "10.0.0.1",
    "ipv4-pool-name": "default"
  },
  "gateway": "10.0.0.2",
  "host-addressing": {
    "ipv4": "192.168.1.100",
    "ipv6": "fd00::100"
  }
}
```

### 5.4 客户端配置

**文件位置**: `pkg/client/client.go`

```go
func NewClient(endpoints []string) *Client {
    transport := http.NewTransport(api.ClientTimeout)
    httpClient := http.NewClient(transport)

    // 使用 Swagger 生成的客户端
    ciliumClient := api.NewHTTPClient(strfmt.Default)
    ciliumClient.Transport = transport

    return &Client{
        HttpClient: httpClient,
        Ipam:       ciliumClient.Ipam,
        Endpoint:   endpoints,
    }
}
```

### 5.5 Agent API 端点配置

**文件位置**: `pkg/api/server.go`

```go
// API Server 监听地址
var (
    // APIPath 是 Unix socket 路径
    APIPath = "/var/run/cilium.sock"

    // APIPort 是 HTTP 端口（如果使用 TCP）
    APIPort = 9876
)

// 注册 IPAM API Handler
func (s *Server) registerIPAMHandlers() {
    s.ipamPostIpamHandler = ipamapi.IpamPostIpamHandler{
        Ipam:                    s.ipam,
        Logger:                  s.logger,
        DefaultExpirationTimeout: 5 * time.Minute,
    }
}
```

---

## 6. 多池 IPAM 模式

### 6.1 CiliumPodIPPool CRD

**文件位置**: `pkg/k8s/apis/cilium.io/v2alpha1/ippool_types.go`

```go
// CiliumPodIPPool 定义可用于池化 IPAM 的 IP 池
type CiliumPodIPPool struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec IPPoolSpec `json:"spec"`
}

// IPPoolSpec 定义 IP 池规范
type IPPoolSpec struct {
    // IPv4 指定池的 IPv4 CIDR 和掩码大小
    IPv4 *IPv4PoolSpec `json:"ipv4,omitempty"`

    // IPv6 指定池的 IPv6 CIDR 和掩码大小
    IPv6 *IPv6PoolSpec `json:"ipv6,omitempty"`

    // PodSelector 选择有资格从此池接收 IP 的 Pod
    PodSelector *slimv1.LabelSelector `json:"podSelector,omitempty"`

    // NamespaceSelector 选择有资格使用此池的 Namespace
    NamespaceSelector *slimv1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// IPv4PoolSpec 定义 IPv4 池
type IPv4PoolSpec struct {
    // CIDRs 是池的 CIDR 列表
    CIDRs []PoolCIDR `json:"cidrs"`

    // MaskSize 是从 CIDR 分配的子网掩码大小
    MaskSize uint8 `json:"maskSize"`
}

// PoolCIDR 定义带范围的 CIDR
type PoolCIDR struct {
    CIDR string `json:"cidr"`
    // 从 CIDR 的哪个 IP 开始（可选）
    Start string `json:"start,omitempty"`
    // 到 CIDR 的哪个 IP 结束（可选）
    End string `json:"end,omitempty"`
}
```

### 6.2 IP 池选择逻辑

**文件位置**: `pkg/ipam/metadata/manager.go:125`

```
开始
  │
  ▼
┌─────────────────────────┐
│ 检查 Pod 注解            │
│ - ipam.cilium.io/ipv4-pool│
│ - ipam.cilium.io/ipv6-pool│
└───────────┬─────────────┘
            │
      找到了?
    ├─YES─→ 返回该池
            │ NO
            ▼
┌─────────────────────────┐
│ 检查 Namespace 注解      │
│ - ipam.cilium.io/ipv4-pool│
│ - ipam.cilium.io/ipv6-pool│
└───────────┬─────────────┘
            │
      找到了?
    ├─YES─→ 返回该池
            │ NO
            ▼
┌─────────────────────────┐
│ 遍历所有 CiliumPodIPPool│
│ 匹配 PodSelector         │
│ 匹配 NamespaceSelector   │
└───────────┬─────────────┘
            │
      匹配数量?
    ├─0 ───→ 返回默认池
    ├─1 ───→ 返回匹配的池
    └─>1 ─→ 返回错误（冲突）
```

### 6.3 IP 池示例

```yaml
apiVersion: cilium.io/v2alpha1
kind: CiliumPodIPPool
metadata:
  name: team-a-pool
spec:
  ipv4:
    cidrs:
      - cidr: "10.10.0.0/16"
        maskSize: 24
  selector:
    podSelector:
      matchLabels:
        team: a
    namespaceSelector:
      matchLabels:
        environment: production
---
apiVersion: cilium.io/v2alpha1
kind: CiliumPodIPPool
metadata:
  name: default-pool
spec:
  ipv4:
    cidrs:
      - cidr: "10.0.0.0/16"
        maskSize: 24
```

### 6.4 通过注解指定 IP 池

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-pod
  annotations:
    ipam.cilium.io/ipv4-pool: "team-a-pool"
    ipam.cilium.io/ipv6-pool: "ipv6-pool-1"
spec:
  containers:
  - name: container
    image: nginx
```

---

## 7. IP 释放机制

### 7.1 释放流程

```
CNI DEL 调用
    │
    ▼ plugins/cilium-cni/cmd/cmd.go:952
Cmd.Del()
    │
    ├─ 调用 EndpointDeleteMany
    │
    │ [pkg/client/ipam.go]
    ▼ client.IPAMRelease()
    │
    ▼ DELETE /ipam/{ip}
    │
    │ [pkg/ipam/api/ipam_api_handler.go]
    ▼ IpamDeleteIpamHandler.Handle()
    │
    │ [pkg/ipam/crd.go:942]
    ▼ crdAllocator.Release()
    │
    ├─ 从 allocated 映射中删除
    │
    ├─ 触发 CiliumNode 更新
    │
    ▼ 返回
```

### 7.2 IP 释放握手机制

为了防止在 IP 仍被使用时错误释放，Cilium 使用握手机制：

```go
// 文件: pkg/ipam/crd.go:443
func (n *nodeStore) handleReleaseHandshake(allocator ipamTypes.Family) error {
    n.mutex.Lock()
    defer n.mutex.Unlock()

    // 根据地址族选择使用的映射
    var usedMap, releaseMap ipamTypes.AllocationMap
    if allocator == ipamTypes.IPv4 {
        usedMap = n.ownNode.Status.IPAM.Used
        releaseMap = n.ownNode.Status.IPAM.ReleaseIPs
    } else {
        usedMap = n.ownNode.Status.IPAM.IPv6Used
        releaseMap = n.ownNode.Status.IPAM.IPv6ReleaseIPs
    }

    // 遍历所有在释放握手中��� IP
    for ip, status := range releaseMap {
        switch status {
        case ipamOption.IPAMMarkForRelease:
            // 检查分配器是否仍在使用此 IP
            if _, ok := allocator.allocated[ip]; ok {
                // IP 仍在使用，NACK 释放
                releaseMap[ip] = ipamOption.IPAMDoNotRelease
            } else {
                // IP 可释放，ACK 释放
                releaseMap[ip] = ipamOption.IPAMReadyForRelease
            }

        case ipamOption.IPAMDoNotRelease:
            // 已被 NACK，从释放握手列表中移除
            delete(releaseMap, ip)

        case ipamOption.IPAMReadyForRelease:
            // 已被 ACK，从 Spec 和 Status 中移除
            delete(releaseMap, ip)
            delete(usedMap, ip)
        }
    }

    return nil
}
```

### 7.3 释放状态转换图

```
┌─────────────────────────────────────────────────────────────┐
│                    IP 释放状态转换                            │
└─────────────────────────────────────────────────────────────┘

  正在使用 (Status.IPAM.Used)
       │
       │ Cilium Operator 标记为释放
       ▼
  ┌─────────────────────────┐
  │ mark-for-release        │ ◄─── Cilium Operator 请求释放
  │ (Status.IPAM.ReleaseIPs) │
  └───────────┬─────────────┘
              │
              │ Cilium Agent 检查
              ▼
     ┌────────┴────────┐
     │                 │
  仍在使用           未使用
     │                 │
     ▼                 ▼
  ┌─────────────────┐  ┌─────────────────┐
  │ do-not-release  │  │ ready-for-release│
  └────────┬────────┘  └────────┬────────┘
           │                    │
           │                    │
           ▼                    ▼
     从列表移除          从 Status 和 Spec 移除
     (保留 IP)            (IP 可重用)
```

---

## 8. 关键代码位置

### 8.1 核心文件

| 文件路径 | 主要功能 |
|----------|----------|
| `plugins/cilium-cni/cmd/cmd.go` | CNI 插件入口，处理 ADD/DEL 命令 |
| `pkg/client/ipam.go` | IPAM HTTP 客户端封装 |
| `pkg/ipam/api/ipam_api_handler.go` | IPAM API 请求处理 |
| `pkg/ipam/ipam.go` | IPAM 核心逻辑，分配器接口 |
| `pkg/ipam/allocator.go` | IPAM 分配器实现 |
| `pkg/ipam/crd.go` | CRD 模式分配器 |
| `pkg/ipam/types/types.go` | IPAM 类型定义 |
| `pkg/ipam/option/option.go` | IPAM 常量和选项 |
| `pkg/ipam/metadata/manager.go` | 多池 IPAM 管理器 |
| `plugins/cilium-cni/types/types.go` | CNI 配置类型 |

### 8.2 关键函数位置

| 函数名 | 文件:行号 | 功能 |
|--------|-----------|------|
| `Cmd.Add()` | `plugins/cilium-cni/cmd/cmd.go:523` | CNI ADD 入口 |
| `Cmd.Del()` | `plugins/cilium-cni/cmd/cmd.go:952` | CNI DEL 入口 |
| `allocateIPsWithCiliumAgent()` | `plugins/cilium-cni/cmd/cmd.go:172` | 调用 Agent 分配 IP |
| `allocateIPsWithDelegatedPlugin()` | `plugins/cilium-cni/cmd/cmd.go:221` | 调用委托插件 |
| `IPAMAllocate()` | `pkg/client/ipam.go:18` | HTTP 分配请求 |
| `IPAMRelease()` | `pkg/client/ipam.go:53` | HTTP 释放请求 |
| `IpamPostIpamHandler.Handle()` | `pkg/ipam/api/ipam_api_handler.go:39` | API 请求处理 |
| `IPAM.AllocateNextWithExpiration()` | `pkg/ipam/ipam.go:243` | IPAM 分配主逻辑 |
| `crdAllocator.AllocateNext()` | `pkg/ipam/crd.go:966` | CRD 分配 |
| `crdAllocator.Release()` | `pkg/ipam/crd.go:942` | CRD 释放 |
| `nodeStore.allocateNext()` | `pkg/ipam/crd.go:635` | 从 CRD 读取 IP |
| `handleReleaseHandshake()` | `pkg/ipam/crd.go:443` | 释放握手处理 |
| `GetIPPoolForPod()` | `pkg/ipam/metadata/manager.go:125` | 选择 IP 池 |

### 8.3 CRD 定义文件

| CRD | 文件路径 |
|-----|----------|
| CiliumNode | `pkg/k8s/apis/cilium.io/client/crds/v2/ciliumnodes.yaml` |
| CiliumPodIPPool | `pkg/k8s/apis/cilium.io/client/crds/v2alpha1/ciliumpodippools.yaml` |

---

## 9. 总结

### 9.1 IPAM 工作流程关键点

1. **CNI 插件不直接分配 IP**，而是通过 REST API 调用 cilium-agent
2. **CiliumAgent 是 IPAM 的实际执行者**，维护 IP 分配状态
3. **CiliumNode CRD 是 IP 状态的存储**，包含可用池和已使用列表
4. **CiliumOperator 负责填充 IP 池**，从 PodCIDR 或配置的 CIDR 中提取
5. **释放握手机制**确保 IP 不会在仍被使用时错误释放

### 9.2 各 IPAM 模式对比

| 模式 | IP 来源 | 管理方 | 适用场景 |
|------|---------|--------|----------|
| `kubernetes` | PodCIDR | Cilium | 标准集群 |
| `crd` | CiliumNode.Spec.IPAM.Pool | Cilium | 需要精确控制 |
| `eni` | AWS ENI | AWS | AWS EKS |
| `multi-pool` | CiliumPodIPPool CRD | Cilium | 多租户隔离 |
| `cluster-pool` | 集群范围池 | Cilium | 简化部署 |
| `delegated-plugin` | 外部 CNI 插件 | 第三方 | 与现有 CNI 集成 |

### 9.3 调试技巧

```bash
# 查看 CiliumNode CRD
kubectl get ciliumnode -o yaml

# 查看 PodIPPool CRD
kubectl get ciliumpodippool -o yaml

# 查看 CNI 配置
cat /etc/cni/net.d/05-cilium.conflist

# 查看 CNI 日志
tail -f /var/run/cilium/cilium-cni.log

# 查看 Cilium Agent 日志
kubectl logs -n kube-system cilium-xxxxx
```
