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
- `allowBtcIndexMismatch` (bool)
  - 含义：**逃生阀**，默认 `false`（不写即关闭）。关闭时，`GetBtcHeader` / `GetBtcHeaderByHash`
    会拿共识状态（statedb 的 `btc-chainstate` 窗口与 `btc-lastheader`）交叉校验 localdb
    （`LODB-lightclient-btc-header-*`，节点私有的逐高度头索引）：
    - localdb 在**共识 tip 高度**上的头必须与 `btc-lastheader` 一致（丢库 / 落后 / 停在别的链上 → 拒）；
    - 请求高度落在 canonical 窗口里时，localdb 的头必须与窗口节点的 hash 一致
      （头 hash 已承诺 merkleRoot，校验 hash 即校验 merkleRoot）→ 不一致即拒。
    拒绝是 **fail-closed**：本节点拒收依赖该高度的区块 / 充值证明，表现为**本节点掉队停机**
    （错误码 `ErrBtcLocalIndexMismatch` / `ErrBtcHeaderNotCanonical`），不是全网静默分叉。
    窗口里没有该高度（比窗口更老 / 窗口尚未写入的老链）**不做判定**，维持原有行为；节点在
    该高度上**没有** localdb 数据时仍是原来的"取不到"语义（`ErrNotFound`），不报成不一致。
  - 打开（`true`）后：不一致只打一条 ERROR 日志，查询照原样返回。**只用于确认要做冷修
    （重建本地索引）时的临时手段**，修好后必须改回 `false` —— 打开期间"区块是否合法"重新变成
    依赖节点私有数据。
  - 恢复（localdb 丢 / 被判定不一致时）：**目前没有按需重建的入口**，需要按下面的现状处置。
    - 重启不会重放：chain33 启动只执行新块，不会重跑历史区块的 `ExecLocal`。
    - 已有的重建通道都是**版本升级触发**的（都会重跑每个区块的 `ExecLocal`，从而重建 dapp localdb）：
      `blockchain.reindex` 路径（`LocalDBMeta` 大版本变化时 `delAllKeys` + 逐高度重放，
      但 `delAllKeys` **不含** `LODB-<execer>-` 前缀，即不删 dapp localdb）；
      以及 `[blockchain] enableReExecLocal=true` 配合 `StoreDBMeta` 大版本变化触发的 `ReExecBlock`
      （见 chain33 `blockchain/reindex.go`、`restore.go` 与 `blockstore.AddTxs`）。两个版本号都是
      chain33 二进制里的常量，运维无法自行触发。
    - 因此：真发生 localdb 丢失，当前只能等一次带版本升级的发布（或临时打开本逃生阀让节点带病运行），
      "扫链上 `BtcHeaders` 交易重建本地索引"的最小重建工具列为后续项（代价：全链区块扫描 + 复用
      `btcHeadersLocalKV` 的同一套 KV 逻辑，需要一次 E2E 验证）。
    - 另注意 localdb 与 statedb 是**两次独立提交**（执行器先落 localdb、blockchain 后落 statedb），
      崩溃窗口内 localdb 可能领先于 statedb —— 那是正常形态（本校验只看共识 tip 高度那一条，
      不受影响）。

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
- **头链提交起点 = 本网络最高锚点 + 1**（没有锚点的网络 → `1`）。这是**本地推导**出来的，不是配置项：
  中继与执行器共用 `ltypes.GetBtcChainParams`，各自从同一份 chaincfg checkpoint 表算出同一个值
  （见 4.1.1）。
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
锚点高度列表、首个头的高度、以及期望的首个提交高度（= 最高锚点 + 1）。注意执行器侧这条信息里的日志键名
仍是 `expectedBtcHeaderStartHeight`（历史名，该项已不是配置项，见 4.1.1）——它给出的值应当与中继本地推出
的起点一致；两边对不上说明中继与执行器**不是同一个 build**。

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
- `minBtcConfirmations` (int64)
  - 含义：**链上最小确认数 N（B8）**。充值证明 / 提现确认证明所在 BTC 区块，必须在 lightclient 的
    canonical 头链里被确认至少 N 个块，否则执行器拒绝该交易（`ErrInsufficientBtcConfirmations`）。
  - 默认行为：未配置或 ≤ 0 一律取 **6**（不允许用 0 表达"不校验深度"）。
  - 影响：N 越大，"头链看得见的深度"越深，重组导致 mint-on-orphan 的概率越低，但**充值到账更慢约
    N 个 BTC 块**（主网 ~10N 分钟）。见下面的"中继侧如何使用 N"。
  - **中继侧如何使用 N（N 的单一真相）**：中继**不镜像**这个值，而是经 lightclient 执行器的只读查询
    `GetRgbxMinBtcConfirmations` 直接读链上生效值（执行器读的就是本节这一段配置，取值口径一致）。
    中继不做本地副本 ⇒ 不存在"两处配置人肉配套"，改这里并重启主链即对中继生效。
    - 中继读不到 N（主链未就绪、或主链执行器还是旧版本、没有这个查询）→ **不提交充值**（fail-closed，
      限流 WARN，下个轮询周期重试）：算不出深度就不提交，绝不用猜测值凑门控。
    - CI 里把 N 配成 `1`（`rgbx/cmd/ci/docker-compose.sh`）：那是为了让 E2E 的"1 个后续块即确认"时序
      成立，**会掩盖"提交那一刻链上可见深度是 0"这类交互问题**（N=1 时深度 0 也够），生产不要照抄。

#### 2.2.1 中继提交充值的深度门控（为什么充值要先等几个块）

中继把 BTC 头链提交到主链时，**只提交到 `best - B`**（B = 4.1 的 `blockConfirmations`，见
`bitcoin.go` 的 `btcConfirmedHeight`）。而 B8 要求 `链上 canonical tip >= H + N - 1`（H = 付款交易所在
BTC 高度）。两者相减 ⇒ 中继**刚收到充值通知就提交**的话，那一刻链上可见深度是 0，首笔提交必然被拒。

因此中继在提交前先做本地门控，判据：

```
best >= H + B + N - 1          （等价于：提交后链上可见深度 >= N）
```

- **正常路径因此不再产生"注定被链上拒"的提交**：B8 退回为纯兜底（本地判据万一算漏时的最后一道闸）。
- 代价是**充值到账延迟 ≈ N 个 BTC 块**（主网 ~10N 分钟）。这是与 B8 相同的时延，只是从"被拒后重试"
  变成"等够了只提交一次"；N 取值是延迟与重组风险的取舍（见 2.2）。
- 门控过不去时中继**不驱动签名轮次**（那是最贵的一步，一轮 GG18 30s 起）、也不提交，只在下个 30s
  轮询重新判断，直到 best 长够。
- 头链本身落后（例如刚 bootstrap、或头链提交停滞）时本地判据可能偏乐观：那种情况下链上仍会按 B8
  拒绝，重试走"只重发"（见 4.4.2），代价可控。

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
  - 说明（头链保留深度 B）：中继提交头链时只提交到 `best - B`，因此"提交那一刻链上可见的 tip"
    也由它决定。**B 与链上 `[exec.sub.rgbx].minBtcConfirmations`（N）是同一个门控的两个参数**：
    中继提交充值前要求 `best >= H + B + N - 1`（见 2.2.1），改动任一个都会改变充值到账时延。
  - 默认行为：未设置时为 6。
- `maxUtxoRescanTime` (int64)
  - 含义：UTXO 重扫超时（单位：小时）
  - 特性：0 表示不超时；内部会转为秒

#### 4.1.1 头链提交起点（无配置项）

- 含义：链上头链还是空（`tip==0`）时，中继从哪个高度开始提交 BTC 头。头链长起来之后起点由"与链上对账"
  得出，这个起点就不再参与（见中继的对账逻辑）。
- 取值：**本网络最高锚点 + 1**（锚点由 btcd 内置，见 2.1.1；mainnet 当前 `810000` → `810001`、
  testnet3 `2344474` → `2344475`）；没有锚点的网络（regtest/testnet4/signet/simnet）→ `1`（创世之后
  第一个块）。
- **不需要配置，也没有对应的配置项**：中继与执行器共用 `ltypes.GetBtcChainParams`，各自从同一份 chaincfg
  checkpoint 表算出同一个值。此前那个 `btcHeaderStartHeight` 配置项已删除；老 TOML 里留着它**无害**
  （解析时被未知键忽略，也不影响起点推导），不必先清理配置文件。
- **启动期断言（L2）**：链上头链还是空时，中继会向主链查询本网络最高锚点，并断言"本地推出的起点 ==
  锚点高度 + 1"：
  - 一致 → 正常提交头；
  - 不一致 → **限流 ERROR（同一状态只报一次）+ 不发交易（fail-closed）**。这说明中继与主链执行器
    **不是同一个 build / 不是同一版 btcd**（两边的锚点表不同源）；日志里给出链上锚点与它期望的起点，
    让两边同源（升级 btcd / 换同一个 build）后重启即恢复；
  - 查询不被支持（主链是旧版执行器，`ErrActionNotSupport`）或查询失败 → WARN 一次后照常提交，
    **不做硬依赖**（升级主链后自动生效）；
  - 本网络没有锚点 → 跳过断言（那种网络只能从创世起，由执行器的锚点校验兜底）。
- **不是钱包重扫的下限**：钱包能看见哪些 BTC 交易由它自己的生日（`btcwallet.db` 创建时刻）决定，与起点
  无关；中继的读/重放窗口是"有 watermark 用 watermark，没有就从钱包自己的最早点读起"，**不设最低高度**
  （理由见 4.1.2）。

#### 4.1.2 钱包交易重放窗口（无配置项）

- 中继启动时按 `min-pending-height`（水位线，落盘在 `neutrino.db` 的 `rgbx-btcwallet-monitor` bucket）
  决定从哪个高度重放钱包自己的交易：
  - **有水位线** → 从水位线重放（停机重启后补齐停机期间的交易）；
  - **没有水位线**（全新节点 / 只清了 `neutrino.db`）→ **从钱包自己的最早点读起（高度 0，不设下限）**。
    从 0 读不贵：tx store 按"有交易的块"走游标，成本与区间长度无关。
- **为什么不留"最低高度"**：钱包能"看见"哪些 BTC 交易完全由它的生日决定（生日 = `btcwallet.db` 创建
  时刻，neutrino 的 rescan 用 `StartTime=生日` 截断，生日之前的块根本不会被下载 filter），任何最小值都
  扩大不了"发现历史充值"的能力；而生日 ≥ 桥启动 ≥ 锚点+1 意味着这类下限永远不会 binding（恒为常量的
  下限没有信息量），钱包从备份恢复时（历史早于那个常量）反而会**截断本该重放的历史**。
- **推论（重要）**：全新节点首启**看不到首启之前**的充值（生日 = 现在），这与任何配置无关；`btcwallet.db`
  被清/重建同样看不到（且旧 TSS UTXO 对钱包不可见，那部分 BTC 桥上花不出去），**只能从备份还原
  `btcwallet.db`**。唯一靠重放能救的是"只清了 `neutrino.db`、钱包 DB 还在"。

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
- `allowShareMismatch` (bool)
  - 含义：**逃生阀**，默认 `false`（不写即关闭）。见 4.3.1：启动自检发现本地 share 与链上组公钥
    不一致时，关闭（默认）即**拒绝启动**；打开后只打 ERROR 日志、照常启动。
  - 只在为了临时把节点拉起来做冷修时打开，修好必须改回 `false`。

#### 4.3.1 启动自检：share ↔ 链上组公钥一致性（无配置项，逃生阀见 4.3）

- 含义：中继启动时（`client.Start()` 最开始，早于 TSS/RGBX 任何后台流程）读本地持久化的 DKG 结果
  （`neutrino.db` 的 bucket `rgbx-tss` / key `dkg-result`，即该节点的 share 对应的组公钥），
  对每个已知 symbol（`BTC` + `[rpc.sub.light.neutrino.rgb20]` 里注册的每个 `contracts.symbol`）
  查链上 `CrossChainInfo`，不一致即 **ERROR + 拒绝启动**（panic，节点退出）。
- 为什么 fail-closed：链上 `CrossChainInfo` **每个 symbol 只有一份、且写死不可改**（re-DKG 换出的
  新组公钥提交上去会被 `checkCommitDKG` 按 `duplicate` 拒掉，而中继把 `duplicate` 当作"已提交"放过，
  见 `submitMainChainTxUntilSuccess`）。于是 share 与链上不一致的节点仍能"参与签名"，但产出的签名
  链上/其他节点不认——**表现是静默失灵而不是报错**（提现卡住、充值签名无效），所以默认拒绝启动。
- 典型成因：share 丢失后自动 re-DKG、换过钥、或从别的环境恢复过数据（`neutrino.db` 与链不是同一代）。
- 比对口径（两边字段形态不同，按可得判据退化）：
  - 链上 `CrossChainInfo.pubkey` 非空（RGB20 symbol 走这条）→ 比对压缩公钥逐字节相等；
  - 链上没有 pubkey（BTC：`CommitDKG` 不带该字段）→ 退化为比对
    `hash160(本地组公钥) == pkScript[2:]`（与链上 `checkCommitDKG` 同一口径，与网络无关）；
  - 链上两者都没有 → 没有判据，不判定。
- **三种情形不判定**（不把正常的当异常）：① 链上还没有该 symbol 的 `CrossChainInfo`（DKG 尚未
  commit，或主链查询不可用/不支持）；② 本地还没有 DKG 结果（首次启动、DKG 还没跑）；③ 本地 DKG
  记录读出来解不开（损坏；那条路径的既有处置是重新 DKG，不在这里改它的行为）。
- 查询有界：自检在主链 grpc 上带 8s 超时 + 最多 2 次尝试（只对传输层错误重试），**不会因为主链
  `QueryChain` hang 而卡住启动**；查不到按"不判定"处理。
- 逃生阀：`tss.allowShareMismatch=true`（见 4.3）。打开后不一致只打一条 ERROR 日志、照常启动——
  **打开期间该节点"参与签名但不被认"，只是把静默失灵换成了可运行的静默失灵**，仅用于临时拉起来冷修。
- 恢复办法（真出现不一致时）：把**与链上同一代**的 `neutrino.db` 还原回去（备份里有就还原），
  或重建整组（清链重跑 DKG）。**重新 DKG 不能修**：新组公钥上不了链（同 symbol 的
  `CrossChainInfo` 已被占死）。

### 4.4 `[rpc.sub.light.neutrino.rgb20]`

RGB20（跨链 USDT）桥的侧车/合约配置（`sidecarAddr` / `consignmentListen` / `contracts` / `precision` /
`changeAddress`）见 `RGB_USDT_INTEGRATION.md`；本节只描述充值重试相关的行为。唯一的额外配置项是
`testSignPsbt`（默认 `false`）：打开后允许 E2E 的 `sign-psbt` 测试端点用 TSS 组签名**任意** PSBT
（签名节点无从核对被签内容），**生产环境必须保持关闭**；`sign-psbt` 仅用于 E2E 模拟用户付款。
本节的行为都**不需要**与主链 `[exec.sub.rgbx]` 人肉对齐：链上最小确认数 N 由中继直接查链上
（见 2.2）。

#### 4.4.1 充值提交的深度门控（无配置项）

见 2.2.1：充值提交前要求 `best >= H + B + N - 1`（B = 4.1 `blockConfirmations`，N = 链上
`minBtcConfirmations`）。行为要点：

- 不够深时**不签名、不提交**，只在下个 30s 轮询重新判断；够深时**一次提交即成**。
- 到账延迟 ≈ N 个 BTC 块（主网 ~10N 分钟），与 B8 的时延相同，只是改成"等够了再提交一次"。
- 取不到 best（neutrino 还没同步出 best block）或读不到链上 N → 不提交（fail-closed），日志限流
  WARN/ERROR（同一个 receive 只报一次）。

#### 4.4.2 签名落盘、重试优先重发（无配置项）

充值签名轮次产出的完整 `DepositAsset`（含 `thresholdSig`）会**落盘**（与 receive/seal 同一个 KVStore
的顶层 bucket `rgb20-deposit-sig`，key = 付款交易 txid），顺序是**先落盘、再提交**。于是：

- **重试优先重发**这份已签对象，不再驱动签名轮次：不再每 30s 空跑一轮 GG18（签名轮次是整条链路上
  最贵的一步，30s 起）。
- 这份落盘产物是**性能缓存，不是重试的唯一依据**：产物丢了就重新签一轮，记录自己能走通，不会卡死。
  重签是安全的 —— 签的是 `C = sha256(types.Encode(DepositAsset{thresholdSig:nil}))`，内容只有金额 /
  目标地址 / 资产符号 + SPV 证明，没有 nonce、时间戳或 UTXO 选择，所以同一笔充值每次重签得到的 `C`
  逐字节相同；链上还按 txid 去重（`formatDepositUsedTxIDKey`）兜底，不会多铸。
- 铸造成功后产物被清掉（不只增不删）。
- 重发被链上按 btc-txid 去重拒绝（`duplicate deposit proof`）时按**已铸造**处理：这是链上已认过这笔
  付款交易的信号（多半是上次提交其实进了链、只是本地没记上 minted），否则会每 30s 重发一次、永远停在
  settled。
- 重启/崩溃不影响：产物在盘上就继续重发。**落盘失败就不提交**（下一轮重试落盘）；进程若在落盘成功前
  重启，产物随之丢失，下一轮重新签一轮即可（只多花一轮 GG18，不影响正确性）。
- **产物丢失是可自愈的常态**（落盘失败后重启、数据目录被清、从旧快照恢复等原因，导致本地只剩一条
  settled 的 receive 而没有 `rgb20-deposit-sig` 记录）：中继直接走"构造 + 签名 + 落盘 + 提交"的正常
  路径，不需要任何人工干预，也没有需要运维放行的异常态。

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
- **所有节点同一个 build**（中继与执行器共用 btcd 内置锚点表；不同 build 的锚点集合不同，头链会在某个
  高度静默停滞）：头链提交起点 = 本网络最高锚点 + 1 由两边各自本地推出、**不需要配置**（当前 mainnet
  `810000` → `810001`、testnet3 `2344474` → `2344475`；没有锚点的网络 → `1`），配套关系由中继启动期
  断言自动校验（不同源会 fail-closed 并打印链上锚点与它期望的起点）
- `blockConfirmations` 符合环境安全要求（测试可低，生产应高）
- 链上 `[exec.sub.rgbx].minBtcConfirmations`（N）符合上线要求（默认 6）：它就是充值到账延迟
  （≈ N 个 BTC 块）与重组风险的那个旋钮；中继**不需要**配任何镜像值，它直接查链上（见 2.2）
- `btcRPC.host/user/pass/TLS` 与 BTC 节点一致
- 全部 TSS 节点的 `peers/threshold` 一致，且 `rank` 分配无冲突
- 每个节点的 `neutrino.db` 是**与当前链同一代**的那份（启动自检会拒掉与链上组公钥不一致的节点，
  见 4.3.1）；`tss.allowShareMismatch` 保持默认关闭

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
  已经给出本网络已知锚点高度列表与期望的首个提交高度（= 最高锚点 + 1，日志键名见下），照做即可（见 2.1.1）。
  中继侧同一问题会在启动期先报一次 fail-closed 的 ERROR（见 4.1.1 的 L2 断言），不必等到运行期。
  注意该值不是配置项：执行器那条信息里的 `expectedBtcHeaderStartHeight` 是历史键名，它给出的值应当与
  中继本地推出的起点一致（两边同源，见 4.1.1）
- 启动期 ERROR "the locally derived bootstrap start height does not match the on-chain btc checkpoint,
  refusing to submit btc headers"：与上一条同源 —— 中继**本地推出**的起点与主链执行器给出的锚点不配套，
  说明两者**不是同一个 build / 不是同一版 btcd**（锚点表不同源），中继按 fail-closed 不发交易。
  让两边同源（升级 btcd / 换同一个 build）后重启即恢复；日志里的 `onChainAnchorHeight` 与
  `onChainExpectedLocalStartHeight` 说明链上认为该是哪一对值（见 4.1.1）。**没有**配置项可改
- 读不到历史充值（重启后/换机后老充值不出现）：中继只重放钱包自己看得见的交易，而钱包能看见哪些由它的
  生日（`btcwallet.db` 创建时刻）决定 —— 全新节点首启、或 `btcwallet.db` 被清/重建，都看不到更早的充值，
  且**没有任何配置能改变这一点**（唯一恢复途径是从备份还原 `btcwallet.db`，见 4.1.2）
- 提交头数过大（`ErrBtcHeadersTooMany`）：单笔超过 64 个头会被拒（中继自身 batchSize=64，正好在上限，
  且会截断更大的批）。自行写中继脚本时同样要 ≤ 64
- 批内高度重复/跳高（`ErrBtcHeaderDuplicateHeight`）：中继必须按高度逐个 +1 提交，不能跳块或重发
- 充值迟迟不到账（比预期晚几个块）：正常行为，见 2.2.1/4.4.1 —— 中继会等
  `best >= H + B + N - 1` 才提交（延迟 ≈ N 个 BTC 块）。想缩短就把链上 `minBtcConfirmations`（N）
  调小（代价是重组风险，见 2.2），**不要**去改 `blockConfirmations`（它同时是头链保留深度与充值通知
  阈值，改了会同时影响头链提交与既有确认语义）
- 充值一直不提交、日志里 `deposit depth gate: ... confirmations unavailable`：中继读不到链上 N
  （主链未就绪，或主链执行器是旧版本、没有 `GetRgbxMinBtcConfirmations` 查询）。中继按 fail-closed
  不提交（见 2.2），升级主链执行器即恢复，不需要改中继配置
- E2E/CI 里看不出"提交那一刻链上深度是 0"的坑：CI 把 `blockConfirmations=1` 与
  `minBtcConfirmations=1` 一起配（见 2.2 与 4.1），N=1 时深度 0 也够，等于把这条交互掩盖了。本地验证
  必须显式用 N > 1（单测已覆盖，见 `rgb20/deposit_retry_test.go`）
- 启动即退出、ERROR "local tss share does not match the on-chain cross chain info"：该节点的 share
  与链上组公钥不是同一代（share 丢失后 re-DKG / 换过钥 / 从别的环境恢复过数据），按 fail-closed
  **拒绝启动**（见 4.3.1）。恢复办法是还原与链同一代的 `neutrino.db`，或重建整组；**重新 DKG 修不了**
  （同 symbol 的 `CrossChainInfo` 已被占死，新组公钥提交会被按 duplicate 放过）。
  `tss.allowShareMismatch=true` 只是临时把它拉起来（带病运行），不是修复
