# Cilium 在 CNI ADD 阶段如何给 Pod 设置 IP（基于 plugins/cilium-cni 代码）

> 目标问题：**cilium 在为 pod 分配 ip 的时候如何设置这个 pod 的 ip？**
>
> 结论：Pod 的 IP 由 **cilium-cni** 在容器 netns 内对 `eth0`（或 `CNI_IFNAME`）执行 `netlink.AddrAdd()` 写入；IP 本身来自 **cilium-agent 的 IPAM API**（或委托的 IPAM 插件），cilium-cni 负责把它配置到 Pod 网卡并把结果通过 CNI Result 返回。

## 1. 入口与主调用链

入口文件：`plugins/cilium-cni/main.go`

- `main()` → `cmd.PluginMain()`
- `cmd.PluginMain()`（`plugins/cilium-cni/cmd/cmd.go`）
  - 通过 `skel.PluginMainFuncs()` 注册 CNI 回调
  - CNI ADD 时进入：`(*Cmd).Add(args *skel.CmdArgs)`

简化链路：

```text
CNI ADD
  -> plugins/cilium-cni/main.go: main
  -> plugins/cilium-cni/cmd/cmd.go: PluginMain
  -> plugins/cilium-cni/cmd/cmd.go: (*Cmd).Add
      1) 解析 CNI 配置/参数
      2) 向 cilium-agent（或委托 IPAM）申请 IP
      3) 创建 veth/netkit，把一端移入 Pod netns
      4) 在 Pod netns 内给接口配置 IP/路由/规则  <-- 这里真正“设置 Pod IP”
      5) 调用 cilium-agent 创建 Endpoint
      6) 输出 CNI Result
```

## 2. IP 从哪里来：IPAM 分配逻辑

位置：`plugins/cilium-cni/cmd/cmd.go`

在 `(*Cmd).Add()` 中，先连接本机 cilium-agent：

- `client.NewDefaultClientWithTimeout()`
- `getConfigFromCiliumAgent()` 获取 `DaemonConfigurationStatus`（含 `IpamMode` 等）

然后按 IPAM 模式分配地址：

### 2.1 默认：向 cilium-agent 申请（最常见）

函数：`allocateIPsWithCiliumAgent()`（`plugins/cilium-cni/cmd/cmd.go`）

关键调用：

- `client.IPAMAllocate("", podName, ipamPoolName, true)`

返回：`*models.IPAMResponse`，其中包含：

- `ipam.Address.IPV4` / `ipam.Address.IPV6`：分配给 Pod 的地址（可能是 `ip/prefix` 形式）
- `ipam.HostAddressing`：节点侧寻址信息（用于计算网关、路由等）

### 2.2 Delegated Plugin：委托给其他 CNI IPAM

当 `conf.IpamMode == ipamOption.IPAMDelegatedPlugin`：

- `allocateIPsWithDelegatedPlugin()`
  - `cniInvoke.DelegateAdd(ctx, netConf.IPAM.Type, stdinData, nil)` 调用外部 IPAM 插件（如 host-local 等）
  - 把返回的 `Result` 转换为 `models.IPAMResponse`，后续配置逻辑与 agent IPAM 一致

## 3. Pod 网卡如何创建：把一端放进 Pod netns

位置：`plugins/cilium-cni/cmd/cmd.go` 的 `(*Cmd).Add()`

核心步骤：

1. 打开 Pod 的 network namespace（注意：kubelet 会把 netns path 通过 `CNI_NETNS` 传进来）：
   - `ns, err := netns.OpenPinned(args.Netns)`
2. 按 datapath mode 创建一对 link（host <-> container）：
   - veth：`connector.SetupVeth(...)`
   - netkit：`connector.SetupNetkit(...)`
3. 把容器侧那根 link 移入 Pod netns：
   - `netlink.LinkSetNsFd(epLink, ns.FD())`
4. 在 Pod netns 内把临时接口名改为 CNI 要求的接口名（通常是 `eth0`）：
   - `ns.Do(func() error { return link.Rename(tmpIfName, epConf.IfName()) })`

到这里为止，只是把“网卡”放进了 Pod，**还没有给 Pod 配 IP**。

## 4. Pod IP 如何真正写进容器：netlink.AddrAdd

> 这部分就是“如何设置 Pod 的 IP”。

### 4.1 从 IPAM 结果构造 IPConfig / 路由（用户态数据准备）

位置：`plugins/cilium-cni/cmd/cmd.go`

- 把 `ipam.Address.IPV4/IPV6` 写入 endpoint request：
  - `ep.Addressing.IPV4 = ipam.Address.IPV4`
  - `ep.Addressing.IPV6 = ipam.Address.IPV6`
- 调用 `prepareIP()` 把字符串 IP 解析为 `netip.Addr`，并基于 `HostAddressing` 计算：
  - `connector.IPv4Routes(...)` / `connector.IPv6Routes(...)`
  - `connector.IPv4Gateway(...)` / `connector.IPv6Gateway(...)`
- 同时把地址写进 `CmdState`：`state.IP4/state.IP6` 以及对应 routes/rules

### 4.2 进入 Pod netns 配置接口：configureIface() → addIPConfigToLink()

位置：`plugins/cilium-cni/cmd/cmd.go`

在 `(*Cmd).Add()` 中调用：

```go
ns.Do(func() error {
    macAddrStr, err = configureIface(scopedLogger, ipam, epConf.IfName(), state)
    return err
})
```

关键函数：

- `configureIface()`：
  - `netlink.LinkSetUp(l)` 把接口置 UP
  - 对 IPv4/IPv6 分别调用 `addIPConfigToLink(...)`

- `addIPConfigToLink()`：
  - **核心：`netlink.AddrAdd(link, addr)`**

源码位置：`plugins/cilium-cni/cmd/cmd.go:addIPConfigToLink()`

这行的效果是：在 **Pod 的网络命名空间** 内，对 `eth0` 写入 `ip addr add <podIP>/<mask> dev eth0` 等价的配置；因此 Pod 内看到的 IP，就是在这里被设置的。

同时还会：

- `netlink.RouteAdd(...)` 添加路由（来自 `connector.IPv4Routes/IPv6Routes`）
- `route.ReplaceRule/ReplaceRuleIPv6(...)` 写入策略路由 rule（如果有）

## 5. 为什么还要输出 CNI Result？

位置：`plugins/cilium-cni/cmd/cmd.go`

cilium-cni 一边“真的把 IP 配到网卡上”，一边也会把同样的地址/路由填到 `cniTypesV1.Result`：

- `res.IPs = append(res.IPs, ipConfig)`
- `res.Routes = append(res.Routes, routes...)`
- `res.Interfaces = ...`（包含 host 侧与 container 侧接口信息）

最后：`cniTypes.PrintResult(res, n.CNIVersion)`。

这一步是为了符合 CNI 协议：runtime/kubelet 需要拿到“最终网络结果”用于记录与后续 CHECK/DEL 逻辑，但 **Pod IP 实际生效依旧来自前面的 netlink 配置**。

## 6. 补充：某些 IPAM 模式下还要在 Host 配额外路由/规则

当需要在 host 侧做 Endpoint routing（比如云厂商 ENI / Azure / AlibabaCloud，或 delegated IPAM 且开启 `InstallUplinkRoutesForDelegatedIPAM`）：

- `needsEndpointRoutingOnHost(conf)` 返回 true
- 调用 `interfaceAdd(...)`（`plugins/cilium-cni/cmd/interface.go`）
  - 使用 `linuxrouting.NewRoutingInfo(...).Configure(...)` 在 host 侧安装规则/路由

这部分不直接“写 Pod IP”，但决定 host 如何把流量正确送到 Pod。

## 7. 你关心的关键代码点（可直接跳转）

- CNI ADD 主逻辑：`plugins/cilium-cni/cmd/cmd.go: (*Cmd).Add`
- IP 分配：
  - agent：`plugins/cilium-cni/cmd/cmd.go: allocateIPsWithCiliumAgent`
  - delegated：`plugins/cilium-cni/cmd/cmd.go: allocateIPsWithDelegatedPlugin`
- **设置 Pod IP（最关键）**：
  - `plugins/cilium-cni/cmd/cmd.go: configureIface`
  - `plugins/cilium-cni/cmd/cmd.go: addIPConfigToLink`（`netlink.AddrAdd`）
- host 侧额外路由/规则（云 IPAM 常见）：`plugins/cilium-cni/cmd/interface.go: interfaceAdd`
