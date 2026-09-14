# RGBX 配置说明手册（运维/测试）

本文档仅说明配置项及含义，覆盖：

- Chain33 主链配置
- Chain33 平行链配置
- Neutrino 子配置（`rpc.sub.light.neutrino`）
- Bitcoin 节点关键配置

## 1. 配置文件范围

建议按职责拆分为三类配置：

- 主链配置（示例：`chain33.toml`）
- 平行链配置（示例：`chain33.para.toml`）
- BTC 节点配置（示例：`bitcoin.conf` 或等价启动参数）

其中跨链相关核心段落在主链和平行链里分别是：

- 主链：`[exec.sub.lightclient]`、`[exec.sub.rgbx]`
- 平行链：`[rpc.sub.light]`、`[rpc.sub.light.neutrino]`、`[rpc.sub.light.neutrino.btcRPC]`、`[rpc.sub.light.neutrino.tss]`

## 2. Chain33 主链配置项

### 2.1 `[exec.sub.lightclient]`

- `btcNetName`
  - 含义：BTC 网络类型
  - 常用值：`regtest`、`testnet`、`mainnet`
- `commitAddress`
  - 含义：提交 BTC 区块头交易（等 lightclient 交易）的唯一授权地址
  - **必填**：只要配置了 `[exec.sub.lightclient]` 段，`commitAddress` 为空将导致**节点启动失败**
    （启动期直接报错退出），错误信息里会给出配置方法。
  - 原因：BTC 头链是充值的信任根，链上没有任何原生锚点，"谁能写头链"完全由该地址决定；
    留空等于向全网开放 BTC 头写入权（任何人可提交任意头 → 可伪造充值）。因此启动期与
    运行期（`CheckTx`）都按 fail-closed 处理：留空即拒绝一切头部提交。
  - 要求：应为受控地址，且与运维密钥管理策略一致
  - 停用方式：确实不需要该执行器时，**删除 `[exec.sub.lightclient]` 段**并保持
    `[fork.sub.lightclient] Enable=-1`（此时不做必填校验）。
  - 过渡说明：本校验是过渡兜底，待头链锚点/治理方案（B4）落地后 `commitAddress` 计划降级为可选。
- `allowRegtestTimeWarp`
  - 含义：仅用于 regtest 测试场景的时间容错开关
  - 建议：仅在 regtest 打开，生产网络关闭

### 2.1.1 BTC 头链锚点（bootstrap 信任根）

头链在链上**没有原生锚点**，若放任不管就是"谁先提交谁定义这条链"：攻击者把难度位设成网络最大目标
（PoW sanity 只校验"自己的 hash ≤ 自己声明的 Bits"，必过）即可秒挖出第一个头，并在同一个难度调整
窗口内一路自造头，凭空造出承载伪造充值交易的"比特币链"。因此 bootstrap（链上还没有任何 BTC 头）
时的首个头必须锚定到**该网络的真实链**，满足以下之一才被接受：

1. **直接接创世**：高度为 1 且 `previousHash` == 该网络创世 hash（由 btcd `chaincfg` 给出，无需维护）；
2. **命中已知锚点**：`previousHash` == 锚点表中高度 `height-1` 的区块 hash；
3. **localDB 可回溯**：沿 `previousHash` 逐级回查本地已存的头，最终到达创世或某个锚点。

锚点表定义在 `plugin/dapp/lightclient/executor/btcd_validate.go` 的 `btcCheckpointTable`，**不需要人工填表**：

- **来源 = btcd 内置的 chaincfg checkpoint**：格式仍是 `网络 → 高度 → 该高度区块的 hash`（`getblockhash`
  口径），键为 btcd 的 `wire.BitcoinNet`，由 `init()` 按 `ltypes.GetBtcChainParams`（中继侧共用同一份
  net→params 映射）从 `chaincfg.*.Checkpoints` 生成。当前各网络最高锚点：**mainnet `810000`**、
  **testnet3 `2344474`**（regtest/testnet4/signet/simnet 无内置锚点）。
- **锚点随 btcd 版本变化**：`TestBtcCheckpointTableGolden` 钉住了上面两个值，**升级 btcd 会让它变红**——
  那是一次显式事件：锚点集合同时影响执行器**和中继**（neutrino 把自己的头链硬锚在 btcd 的 checkpoint 上，
  同步到该高度时 hash 必须相同，否则断 peer 并回滚头库），需要确认"所有节点同一个 build"（不同 build 的
  锚点不同 → 头链走到该高度必然对不上 → **表现是中继自己不报错、链上头链静默停滞**）。
- **`btcHeaderStartHeight` 应当填 = 本网络最高锚点 + 1**（见 4.1；中继启动期会自己断言这一点）。
- **需要更新鲜的起点时，首选升级 btcd**（那样中继侧锚点一起前进，两边天然一致）；确实不能升级时，
  才用编译期的扩展层 `extraBtcCheckpoints` 手加一条：必须**完整重述** chaincfg 的每一条锚点（同高度同
  hash）、只允许新增**更高**的高度、且不给没有内置锚点的网络（regtest/testnet4/signet/simnet）扩展。
  违反任一条 → 节点启动直接 panic、CI 单测同时变红（fail-closed，不允许带错表跑起来）。
- 该表同时用于同步过程校验：链走到表中高度时头 hash 必须与表一致，否则整条链都不是真实链
  （报 `ErrBtcHeaderVerify`）。
- **regtest 不要填**：regtest 链每次启动都会重建（btcd `removeRegressionDB`），chaincfg 对它没有锚点，
  扩展层也拒绝给它加锚点。

bootstrap（链上还没有任何 BTC 头）时，首个头的父块必须正好是上表里的某个锚点（或能沿 `prevHash` 回溯到
创世/锚点），否则报 `ErrBtcHeaderNoAnchor` 并拒收；**错误与日志会一并给出照做就能过的信息**：本网络已知
锚点高度列表、首个头的高度、以及期望的 `btcHeaderStartHeight`（= 最高锚点 + 1）。

此外，单笔交易（`BtcHeaders` action）允许提交的头数上限为 **64**，且同一笔交易内的头高度必须逐个 +1
（重复高度或跳高都会被拒绝）。中继自身 `batchSize=64`——**正好用满**这个上限（mainnet 内置锚点只到
`810000`，距当前 tip 约 10 万头：16 头/批要追 ~15 天，64 头/批 ~4 天），中继侧也会把更大的批截断到 64。

### 2.2 `[exec.sub.rgbx]`

- `commitAddress`
  - 含义：提交 RGBX 关键交易（如确认交易）的授权地址
  - 要求：应与对应签名私钥匹配
- `crossChainAssetPrefix`
  - 含义：跨链映射资产前缀
  - 示例：`X`，则 BTC 映射资产为 `XBTC`
- `guardianParachainTitle`
  - 含义：受信平行链标题（para title）
  - 要求：与平行链 `Title` 完全一致

## 3. Chain33 平行链配置项

### 3.1 平行链基础配置（按模块归属）

- 根级：`Title`
  - 含义：平行链标题
  - 要求：与主链 `guardianParachainTitle` 一致
- `[rpc.parachain]`：`mainChainGrpcAddr`
  - 含义：主链 gRPC 地址
  - 用途：平行链访问主链查询/提交接口
- `[consensus.sub.para]`：`authAccount`
  - 含义：平行链共识节点账户
  - 用途：节点身份与权限校验（跨链节点需与钱包私钥对应）
- `[crypto]`：`enableTSS`
  - 含义：是否开启 TSS 功能
  - 建议：跨链节点必须开启
- `[p2p]`：`types`、`enable`、`waitPid`
  - 含义：P2P 网络与 DHT 开关配置
  - 建议：开启 DHT（`types=["dht"]`、`enable=true`），保证 TSS 节点发现与消息分发
- `[p2p.sub.dht]`：`DHTDataPath`
  - 含义：DHT 数据目录
  - 建议：配置稳定持久化路径，避免重启后频繁重建邻居关系

### 3.2 `[rpc.sub.light]`

- `clients`
  - 含义：light client 插件列表
  - RGBX 场景固定为：`["neutrino"]`
- `commitAddr`
  - 含义：light RPC 使用的提交地址
  - 要求：与对应提交私钥匹配

## 4. Neutrino 配置项（平行链）

以下字段定义来源于 `plugin/dapp/lightclient/rpc/lightclient/neutrino/config.go`。

### 4.1 `[rpc.sub.light.neutrino]`

- `isOfficialNode` (bool)
  - `true`：启动官方流程（BTC 同步、充值监听、提现处理、确认提交）
  - `false`：不执行官方提交流程，通常用于验证/协作节点
- `maxPeer` (int)
  - 含义：Neutrino 最大对等节点数
  - 默认行为：小于 1 时使用默认值（实现内为 8）
- `blockCacheSize` (uint64)
  - 含义：区块缓存大小（字节）
  - 默认行为：小于 1MB 时使用默认值（约 20MB）
- `netName` (string)
  - 含义：BTC 网络名
  - 要求：与主链 lightclient 的 `btcNetName` 一致
- `addPeers` ([]string)
  - 含义：启动后主动增加的 P2P 节点列表
- `connectPeers` ([]string)
  - 含义：固定连接节点列表
  - 说明：配置后通常只连该列表，不再自动找出站节点
- `btcBlockInterval` (uint32)
  - 含义：BTC 区块同步轮询间隔基准（秒）
  - 默认行为：未设置时采用实现默认值
- `blockConfirmations` (uint32)
  - 含义：BTC 确认数门限
  - 影响：区块头提交、pending 拉取、充值/提现确认时机
- `btcHeaderStartHeight` (uint64)
  - 含义：首次提交 BTC header 的起始高度。**只在链上头链为空时使用**：链上 `tip==0` 时它是提交起点，
    头链长起来之后起点由"与链上对账"得出（见中继的对账逻辑），这个配置值就失效了。
  - 应当填：**本网络最高锚点 + 1**（锚点由 btcd 内置，mainnet 当前是 `810000` → 填 `810001`；
    见 2.1.1）。没有锚点的网络（regtest/testnet4/signet/simnet）填 1（创世之后第一个块）。
  - 默认行为：未设置时回退为 1。**mainnet/testnet3 上不要依赖这个默认值**：从高度 1 起等于从创世同步
    （几百万个头，实际不可用），而且启动期断言会直接报错（见下）。
  - **启动期断言（L2）**：链上头链还是空时，中继会向主链查询本网络最高锚点，并断言
    `btcHeaderStartHeight == 锚点高度 + 1`：
    - 一致 → 正常提交头；
    - 不一致 → **限流 ERROR（同一状态只报一次）+ 不发交易（fail-closed）**，日志里给出
      `expectedBtcHeaderStartHeight`（照做即可），改对配置重启后恢复；
    - 查询不被支持（主链是旧版执行器，`ErrActionNotSupport`）或查询失败 → WARN 一次后照常提交，
      **不做硬依赖**（升级主链后自动生效）；
    - 本网络没有锚点 → 跳过断言（那种网络只能从创世起，由执行器的锚点校验兜底）。
  - 注意：该高度也用作钱包重扫的下限（`monitorTransactions` 的 rescan 起点），改大它意味着
    **更早的 BTC 区块不再重扫**，确认无历史充值遗漏后再调。
- `maxUtxoRescanTime` (int64)
  - 含义：UTXO 重扫超时（单位：小时）
  - 特性：0 表示不超时；内部会转为秒

### 4.2 `[rpc.sub.light.neutrino.btcRPC]`

- `host`
  - 含义：BTC RPC 地址（host:port）
- `user`
  - 含义：BTC RPC 用户名
- `pass`
  - 含义：BTC RPC 密码
- `mode`
  - 含义：RPC 连接模式，支持 `ws`（默认）或 `http`
- `disableTLS` (bool)
  - 含义：是否禁用 TLS
  - 建议：生产环境使用 TLS
- `certFile`
  - 含义：TLS 证书路径（可选）
  - 要求：启用 TLS 时路径可读且证书与 `host` 匹配

### 4.3 `[rpc.sub.light.neutrino.tss]`

- `peers` ([]string)
  - 含义：TSS 参与节点地址列表
  - 要求：所有参与节点配置一致
- `threshold` (uint32)
  - 含义：阈值签名门限（t-of-n 中的 t）
  - 建议：按容错策略设置，且不大于节点总数
- `rank` (uint32)
  - 含义：节点角色标识（用于区分官方/验证角色）
  - 要求：同一节点在全网配置必须稳定一致

### 4.4 `[rpc.sub.light.neutrino.rgb20]`

RGB20（跨链 USDT）桥的侧车/合约配置（`sidecarAddr` / `consignmentListen` / `contracts` / `precision` /
`changeAddress`）见 `RGB_USDT_INTEGRATION.md`；这里只说明与签名侧去重相关的一项。

- `signedDepositTTL` (int64)
  - 含义：**签名侧"已签集合"的保留期（TTL），单位 = BTC 区块数**（不是秒/小时）。
  - 作用：签名节点在签出 `threshold_sig`（= chain33 铸币授权）**成功之后**，把那笔 BTC 付款交易的
    `txid` 记进本地已签集合（顶层 bucket `rgb20-signed-deposit`，与 receive/seal 同一个 KVStore）；
    同一个 `txid` 再来时直接拒绝签名，不再进入签名轮次。付款交易所在 BTC 高度记为锚点，
    `链上 canonical tip 高度 - 记录高度 >= signedDepositTTL` 即视为过期：过期记录被清掉，
    同一 `txid` 允许再次签名。
  - `0` 或 `-1`（任何 ≤ 0 的值，**默认 0**）：**只增不删** —— 已签记录永久保留，永不因 TTL 被清理，
    也不做任何链上高度查询（默认配置下这条机制零查询、零行为变化）。
  - 正数：按 BTC 块数保留。参考值：`144` ≈ 1 天（10 分钟/块）、`1008` ≈ 1 周、`4320` ≈ 1 个月。
    取值越大，同一个 `txid` 能被重复签名的窗口越窄（越保守）；但**过期后同一 txid 会被重新放行**，
    这是 TTL 的固有取舍，链上 txid 去重（`formatDepositUsedTxIDKey`）不受影响，仍是最终兜底。
  - **改配置重启立即生效**：启动时若 TTL 为正，会**立刻做一次清理**，把已过期的旧记录（包括之前
    TTL=0 期间攒下的、或从更大的 TTL 调小后超期的记录）一并删掉；清理在后台执行（取链上高度可能
    因启动竞态失败，会按 3s 间隔重试），不阻塞节点启动。之后在每次标记成功时顺带清理一次。
  - 单位选 BTC 高度而不是时钟：高度由链决定，四个签名节点看到的是同一个、单调的计数，不受本机
    时钟漂移/回拨影响，也不要求节点自己有 neutrino 头库（validator 节点的本地 bestBlock 是空的）。
    代价是 TTL 判定要读一次链上 tip（lightclient 的 `GetBtcLastHeader` 查询，带 5s 超时），
    因此只在 TTL > 0 且确有一条记录要判定时才查。
  - 失败取向（fail-closed）：TTL > 0 但链上高度取不到时，**保持拒绝**（按"仍在保留期"处理），
    不会因为查询失败就放行重复签名。
  - 注意：这条去重是**纵深防御**，不是铸币闸门 —— 即使重复签出 `threshold_sig`，链上也会按 txid
    拒绝第二笔铸造（不多铸）；它挡住的是"给协调者多余的签名产物 + 白跑签名轮次"。

## 5. Bitcoin 节点关键配置项

以下示例使用 `bitcoin.conf` 格式说明关键配置（btcd/bitcoin-core 参数名有差异时，以节点实现文档为准）：

```ini
# 网络（与 Chain33 的 btcNetName/netName 保持一致）
regtest=1
# testnet=1
# mainnet 默认不需要显式开启

# RPC 监听地址（需保证 Neutrino 的 btcRPC.host 可达）
rpcbind=0.0.0.0
rpcport=18443

# RPC 认证（需与 rpc.sub.light.neutrino.btcRPC.user/pass 一致）
rpcuser=root
rpcpassword=1314

# TLS / 证书（若启用 TLS，证书路径需与 Neutrino certFile 对应）
# 常见做法是由节点自动生成证书；若禁用 TLS 则需与 disableTLS=true 匹配
# rpcssl=1
# rpcsslcertificatechainfile=/path/to/rpc.cert

# 交易与地址索引（建议开启）
txindex=1
addrindex=1

# 过滤能力（支持neutrino轻节点）
blockfilterindex=1
peerblockfilters=1
```

字段对照关系：

- `rpc.sub.light.neutrino.btcRPC.host` <-> `rpcbind/rpcport`
- `rpc.sub.light.neutrino.btcRPC.user` <-> `rpcuser`
- `rpc.sub.light.neutrino.btcRPC.pass` <-> `rpcpassword`
- `rpc.sub.light.neutrino.btcRPC.disableTLS/certFile` <-> TLS/证书配置
- `rpc.sub.light.neutrino.netName` <-> `regtest/testnet/mainnet` 网络选择

补充说明（参考 Bitcoin Core Neutrino 模式文档）：

- Bitcoin Core 需支持 BIP157/BIP158（常见要求为 0.21.0+）
- 首次开启 `blockfilterindex=1` 后，节点会重建过滤索引，过程可能较慢
- 如果不希望节点对外自动发现，可在 `bitcoin.conf` 配置 `discover=0`

## 6. TSS 组网说明（当前实现约束）

当前实现建议采用：**1 个官方节点 + N 个第三方节点**（N >= 2，且满足阈值）。

- 官方节点：
  - `rpc.sub.light.neutrino.isOfficialNode=true`
  - 负责发起业务主流程（BTC 同步、充值监听、提现处理、确认提交）
- 第三方节点：
  - `rpc.sub.light.neutrino.isOfficialNode=false`
  - 参与 DKG 和签名协作，不发起官方提交流程
- 角色约定（实践中）：
  - 官方节点 `rank=0`
  - 第三方节点 `rank=1`
- 全体节点必须保持一致：
  - `rpc.sub.light.neutrino.tss.peers`
  - `rpc.sub.light.neutrino.tss.threshold`
- 组网依赖：
  - 平行链需启用 DHT P2P（`[p2p] types=["dht"]` 且 `enable=true`）

## 7. 配置一致性检查清单

上线/联调前建议逐项核对：

- `btcNetName == netName == BTC 节点 network`
- 主链 `guardianParachainTitle == 平行链 Title`
- `commitAddress/commitAddr/authAccount` 与私钥管理匹配
- 主链 `[exec.sub.lightclient].commitAddress` **非空**（留空节点起不来）
- `btcHeaderStartHeight` = **主链执行器那个 build 的"本网络最高锚点 + 1"**（锚点由 btcd 内置，不再手填；
  当前 mainnet `810000` → `810001`、testnet3 `2344474` → `2344475`）。配套关系由中继启动期断言自动
  校验（不一致会 fail-closed 并打印期望值），但**上线前要人工确认所有节点同一个 build**：不同 build 的
  锚点集合不同，头链会在某个高度静默停滞
- `blockConfirmations` 符合环境安全要求（测试可低，生产应高）
- `btcRPC.host/user/pass/TLS` 与 BTC 节点一致
- 全部 TSS 节点的 `peers/threshold` 一致，且 `rank` 分配无冲突

## 8. 最小示例（仅配置片段）

```toml
# main
[exec.sub.lightclient]
btcNetName="regtest"
commitAddress="1xxxxxxxxxxxxxxxx"
allowRegtestTimeWarp=true

[exec.sub.rgbx]
commitAddress="1xxxxxxxxxxxxxxxx"
crossChainAssetPrefix="X"
guardianParachainTitle="user.p.rgbx."

# para root-level
Title="user.p.rgbx."

[p2p]
types=["dht"]
enable=true
waitPid=false

[p2p.sub.dht]
DHTDataPath="paradatadir/p2pstore"

[rpc.parachain]
mainChainGrpcAddr="127.0.0.1:8802"

[consensus.sub.para]
authAccount="1xxxxxxxxxxxxxxxx"

[crypto]
enableTSS=true

[rpc.sub.light]
clients=["neutrino"]
commitAddr="1xxxxxxxxxxxxxxxx"

[rpc.sub.light.neutrino]
isOfficialNode=true
netName="regtest"
connectPeers=["127.0.0.1:18444"]
btcBlockInterval=2
blockConfirmations=1
maxUtxoRescanTime=60

[rpc.sub.light.neutrino.btcRPC]
host="127.0.0.1:18443"
user="root"
pass="1314"
disableTLS=false
certFile="/path/to/rpc.cert"

[rpc.sub.light.neutrino.tss]
peers=["1addrA","1addrB","1addrC","1addrD"]
threshold=3
rank=0 # 官方节点；第三方节点配置为 rank=1，且 isOfficialNode=false

[rpc.sub.light.neutrino.rgb20]
# 签名侧已签集合的保留期（BTC 块数）：0/-1 = 只增不删（默认，不清理）；正数 = 过期后同一 txid 可再签
signedDepositTTL=0
```

第三方节点相对官方节点的最小差异：

- `[rpc.sub.light.neutrino] isOfficialNode=false`
- `[rpc.sub.light.neutrino.tss] rank=1`
- 节点自身 `authAccount` / `commitAddr` 使用本节点受控地址
- `peers` 与 `threshold` 必须与官方节点保持一致

## 9. 常见配置错误

- 网络不一致：`netName` 与 BTC 实际网络不一致导致校验失败
- TLS 不匹配：启用 TLS 但 `certFile` 错误或证书主机名不匹配
- TSS 不一致：节点间 `peers/threshold` 不一致导致 DKG 或签名异常
- 多官方节点：多个 `isOfficialNode=true` 节点并行处理，导致重复提交/状态竞争
- 确认数过低：测试通过但生产抗重组能力不足
- 授权地址不匹配：`commitAddress/commitAddr/authAccount` 与私钥不对应
- `commitAddress` 留空：节点启动失败（`[exec.sub.lightclient].commitAddress must not be empty`），
  见 2.1；不是 bug，是防止头链写入权对全网开放
- 首个 BTC 头被拒（`ErrBtcHeaderNoAnchor`）：bootstrap 起点既不是创世、也没命中锚点表。错误信息里
  已经给出本网络已知锚点高度列表与期望的 `btcHeaderStartHeight`（= 最高锚点 + 1），照做即可（见 2.1.1）。
  中继侧同一问题会在启动期先报一次 fail-closed 的 ERROR（见 4.1 的 L2 断言），不必等到运行期
- 启动期 ERROR "btcHeaderStartHeight does not match the on-chain btc checkpoint, refusing to submit btc
  headers"：与上一条同源，`btcHeaderStartHeight` 与主链执行器的锚点不配套，中继按 fail-closed 不发交易；
  按日志里的 `expectedBtcHeaderStartHeight` 改配置后重启（见 4.1）
- 提交头数过大（`ErrBtcHeadersTooMany`）：单笔超过 64 个头会被拒（中继自身 batchSize=64，正好在上限，
  且会截断更大的批）。自行写中继脚本时同样要 ≤ 64
- 批内高度重复/跳高（`ErrBtcHeaderDuplicateHeight`）：中继必须按高度逐个 +1 提交，不能跳块或重发
- 重复签名被拒（`deposit tx ... already signed by this node`）：该付款交易 txid 已在本地已签集合里
  （见 4.4）。属预期行为，不是故障；确需放行只能等 TTL 到期（或把 `signedDepositTTL` 调小后重启，
  启动清理会立即删掉超期记录）
- `signedDepositTTL` 配了正数却像"没生效"：TTL 以**付款交易所在高度**为锚点，付款高度与链上 tip
  差距超过 TTL 的记录本就已过期（一签字就过期），对这类老付款不提供去重——去重窗口是"付款后
  TTL 个块内"
