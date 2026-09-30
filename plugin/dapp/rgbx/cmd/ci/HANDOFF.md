# BTC 跨链演示环境 · 部署与对接手册

> 交付分支：`demo/btc-crosschain`。读者：需要把整套 BTC 跨链系统跑起来、并对接前端做演示的工程师。

## 一、这是什么

**一条命令，在本地拉起一整套跨链网络**：一条 chain33 主链 + 4 条平行链 + 一条 regtest 比特币链 + 一个 RGB 侧车。起来之后，可以完整演示「用户付 BTC → 链上到账 → 提现回 BTC」。

打个比方：这套环境相当于把交易所的「出入金系统」整个塞进你的笔记本。比特币链是真实的一条链（regtest 模式，块由你自己挖），chain33 是记账那条链，中间那座桥由 4 个节点用**门限签名**共同掌管 —— 没有任何单个节点能单独动这笔钱。

**演示范围**：只演示 BTC 跨链。USDT / RGB20 那套不在演示范围内（本分支默认跳过它们的场景断言）。

## 二、环境拓扑

```mermaid
flowchart TB
    subgraph btc["比特币侧（regtest）"]
        btcd["btcd 全节点<br/>18443 RPC / 18444 P2P"]
    end

    subgraph c33["chain33 侧"]
        main["main 主链<br/>8801 jrpc / 8802 grpc<br/>lightclient 执行器：BTC 头共识校验"]
        para["para1-4 平行链<br/>门限签名节点（para1 official rank=0，<br/>para2-4 validator rank=1，阈值 3）<br/>桥跑在这里"]
        sidecar["rgb-sidecar<br/>50061 gRPC / 50064 test-sim<br/>桥在 BTC 侧的账本与交易构造"]
    end

    user(["用户 / 前端"])
    user -->|"① 领充值地址"| para
    user -->|"② 付 BTC"| btcd
    btcd -->|"③ 桥 watch 到充值"| sidecar
    sidecar --> para
    para -->|"④ 铸 XBTC"| main
    btcd -.->|"BTC 区块头"| main
    para -->|"⑤ 扫集：归集回主池（TSS 签名）"| btcd
```

要点：

- **桥跑在平行链上**（`para1-4` 的 `[rpc.sub.light]`），不在主链。主链只跑 lightclient **执行器**，做 BTC 区块头的共识校验。
- **充值归因不用 OP_RETURN**：每个用户一个 `P2WSH(chain33地址, TSS群公钥)` 派生的 BTC 地址，链上按同一份派生认定归属。
- **BTC 侧的资金由侧车管**：侧车用 RGB seal 表示桥控制的那批 BTC UTXO，并构造扫集/提现交易（所以这套环境有一个 Rust 容器，见 §六）。

## 三、对外端口

| 服务 | 端口 | 用途 | 给谁 |
|---|---|---|---|
| main | `8801` | chain33 JSON-RPC | 前端 / CLI |
| main | `8802` | chain33 gRPC | 前端 |
| para1 | `17001` | **充值地址发放接口（HTTP）** | 前端 |
| para1 | `17000` | RGB consignment 监听 | 一般不直连 |
| btcd | `18443` | 比特币 RPC（regtest；用户 `root` / 密码 `1314`） | 演示脚本挖块、付款 |
| btcd | `18444` | 比特币 P2P | — |
| rgb-sidecar | `50061` | 侧车 gRPC | 调试 |
| rgb-sidecar | `50064` | test-sim（仅 regtest） | 演示脚本 |

## 四、前置条件

- **Docker Desktop**（建议 ≥ 8 核 / 16 GB 内存 / 20 GB 空闲磁盘）
- **Go 1.23**（只用来交叉编译 chain33 二进制）
- **Rust 不需要装在宿主机** —— 侧车在容器里编译

首次启动要编译 chain33 与 RGB 侧车，约 **10–20 分钟**；之后走 Docker 层缓存，几十秒。

## 五、起 / 停

```bash
# 起环境（首次含编译）
make docker-compose proj=up dapp=rgbx

# 停（保留容器与数据卷，下次原地继续）
make docker-compose-down proj=down dapp=rgbx
```

起来后核对：`docker compose -p rgbx ps` 里 `main`、`btcd` 应为 `healthy`，`para1-4`、`rgb-sidecar` 为 `Up`。

`proj=` 后面是动作名：`up`（只起环境）、`run`（起环境 + 跑全部用例）、`down`、`reset`、`test`。

> 想连 RGB20/USDT 一起跑：`RGBX_BTC_ONLY=0 make docker-compose proj=run dapp=rgbx`。

## 六、为什么有一个 Rust 容器

侧车干的是**桥在比特币那一侧的工作**，不是「只给 USDT 用的」：

- 充值地址的脚本登记（`syncDepositScriptsWithSidecar`）
- 扫集交易的构造（`build sweep`）
- 桥在 BTC 侧的账本（用 RGB seal 表示它控制的 UTXO）

实测：把侧车停掉 100 秒，`para1` 不会崩（`net is_sync` 仍为 `true`），但持续报错，**充值地址不再发放、扫集不再发生** —— BTC 那条轨道就断了。所以这个容器去不掉。

## 七、测试账号与地址

以下都是 regtest 的固定测试凭据，**仅用于本地演示，没有任何真实价值**（它们本来就写在 `docker-compose.sh` 里）。

| 用途 | 地址 | 私钥 |
|---|---|---|
| 演示用户（chain33 侧收款地址） | `14KEKbYtKKQm4wMthSK9J4La4nAiidGozt` | — |
| 主链账户 1（演示主账户） | `1KSBd17H7ZK8iT37aJztFB22XGwsPTdwE4` | `0x6da92a63…a81b` |
| 主链账户 2 | `1JRNjdEqp4LJ5fqycUBm9ayCKseeskgMKR` | `0x19c06923…fee4` |
| 主链账户 3 | `1NLHPEcbTWWxxU3dGUZBhayjrCHD3psX7k` | `0x7a80a1f7…f115` |
| 主链账户 4 | `1MCftFynyvG2F4ED5mdHYgziDxx6vDrScs` | `0xcacb1f5d…6b71` |
| genesis / 签名私钥（`-k` 用） | — | `CC38546E…6944` |
| BTC 提现收款地址（regtest） | `bcrt1qnnwpfpljh5n8m3a8xtf3x5ayvhjjplxmhuexyh` | — |
| btcd RPC | `127.0.0.1:18443`，`root` / `1314` | — |

完整私钥见 `docker-compose.sh` 顶部的 `AUTH_KEY1-4` / `GENESIS_KEY`；`USER_MAIN_ADDR` 就是演示用户的收款地址。

**充值地址不是固定的**：每个用户一个，要向桥索取（见 §八 步骤 ①）。

## 八、完整流程：前端接口 ↔ 等价 CLI

### 充值

| # | 动作 | 接口 | 等价 CLI / 命令 |
|---|---|---|---|
| ① | 领该用户的充值地址 | `GET` 或 `POST http://<para1>:17001/rgbx/v1/btc-deposit-address`，参数 `chain33Addr=<用户chain33地址>` | 无（纯 HTTP） |
| ② | 用户付 BTC 到该地址 | 用户自己的 BTC 钱包；演示环境可用 btcd RPC 造交易 | `chain33-cli rgbx btcDepositTx …`（harness 内部用） |
| ③ | 查到账余额 | chain33 jrpc：`asset balance` | `chain33-cli asset balance -a <用户地址> --asset_exec=rgbx --asset_symbol=XBTC` |
| ④ | 查跨链信息（TSS 公钥 / 主池地址） | chain33 jrpc：`rgbx getCrossChainInfo` | `chain33-cli rgbx getCrossChainInfo -s BTC` |
| ⑤ | 扫集（归集回主池） | **桥自动**（空闲 ticker），无需前端调用 | — |

**① 的返回**（已按契约核实）：

```json
{"data":{"address":"bcrt1q…","pkScript":"…","userID":"…","spec":"P2WSHDepositSpecV1","watchSize":1}}
```

- **幂等**：同一用户重复请求返回同一地址，watch 集不增长。
- 缺参数 / 非法地址 → `400`；方法不对 → `405`。
- **绝不返回一个没被 watch 的地址**（fail-closed）—— 所以地址必须**向桥要**，本地自己推导的地址（`chain33-cli rgbx btcDepositAddress`）桥不会去 watch，只能用来对账。

链上记账符号是 **`XBTC`**：源资产符号是 `BTC`，跨链资产前缀 `X` 由主链配置项 `crossChainAssetPrefix` 决定。

### 提现

| # | 动作 | 接口 | 等价 CLI |
|---|---|---|---|
| ① | 发起提现 | chain33 jrpc：`rgbx withdraw` | `chain33-cli send rgbx withdraw -a <金额> -f <费率 sat/vB> -d <收款BTC地址> -s BTC -k <签名私钥>` |
| ② | 查提现进度 | chain33 jrpc：`rgbx listPendingTx` | `chain33-cli rgbx listPendingTx -s 0 -i 0 -c 20` |
| ③ | 广播到 BTC、确认、销毁 | **桥自动**（TSS 签名 → 广播 → 确认 → 链上销毁） | — |

### 链上流转（不跨链）

用户之间在 chain33 上转 XBTC —— 纯链上动作，不涉及 BTC 侧。

| # | 动作 | 接口 | 等价 CLI |
|---|---|---|---|
| ① | 转账 | chain33 jrpc：`rgbx transfer` | `chain33-cli send rgbx transfer -a <金额> -s XBTC -t <收款chain33地址> -k <私钥>` |
| ② | 查余额 | chain33 jrpc：`asset balance` | `chain33-cli asset balance -a <地址> --asset_exec=rgbx --asset_symbol=XBTC` |

> ⚠️ 注意符号不一致：`transfer` 用链上符号 **`XBTC`**，`withdraw` 用源资产符号 **`BTC`**。这是当前实现的实际行为，前端按各自命令抄即可。

**给前端的提示**：`chain33-cli send …` 内部是**三步**，前端用 jrpc 时要自己拼同一条链：

1. `Chain33.CreateTransaction`（`execer=rgbx`、`actionName=WithdrawAsset`、payload 见 `WithdrawAsset`）
2. `Chain33.SignRawTx`（用用户私钥）
3. `Chain33.SendTransaction`

金额按资产的十进制精度给出（BTC 是 8 位小数）。桥接的 RGB20 资产另需显式给精度（CLI 的 `-p`）。

## 九、已知限制

1. **只演示 BTC 轨道**：本分支跳过 RGB20/USDT 的场景断言（`RGBX_BTC_ONLY=1`，本分支默认）。
2. **提现与链上流转的回归覆盖**：这两条旧场景（`scenario_user_withdraw_auto_confirm` / `scenario_user_transfer_crosschain_asset`）本分支已单独启用（见 `testcase.sh` 的 `RGBX_BTC_ONLY=1` 分支）。**2026-09-30 实测通过**：提现那笔在 BTC 侧真实落地。注意它们的断言沿用 P2WPKH 时代的写法，路径若再变需要重新核对。
3. **regtest 环境**：比特币是本地私链，块要自己挖；金额、确认数都是演示口径，不是生产参数。
4. **`testSignPsbt` 在 regtest 下打开**（para toml）：该端点等于「用组私钥签任意内容」，生产必须关。
5. **备份/恢复**：TSS share 与侧车账本**不可再生**（丢了就永久失去签名能力）。演示环境可以整环境重建；按这个架构上生产前必须先落地备份方案（见仓库内 `BACKUP_RECOVERY.md`）。
6. **测试私钥**：§七 那些私钥写在公开仓库里，只适用于 regtest。**不要把它们用到 testnet / mainnet。**

## 十、排障

| 症状 | 先看什么 |
|---|---|
| `para1-4` 起来就退出 | 桥的 fail-closed 校验（合约精度 / 身份）。日志搜 `refuse to start` |
| 充值不到账 | 侧车在不在、地址是不是**向桥要的**、确认数够不够 |
| 提现卡住 | `rgbx listPendingTx` 看 pending；`para1` 日志搜 `withdraw` |
| 余额不涨 | 确认符号用 `XBTC`（不是 `BTC`） |

```bash
docker logs --tail=200 rgbx-para1-1          # 桥在平行链上
docker logs --tail=200 rgbx-main-1           # 主链 / lightclient 执行器
docker logs --tail=200 rgbx-rgb-sidecar-1    # 侧车
```

## 十一、相关文档

- `plugin/dapp/lightclient/rpc/lightclient/neutrino/CONFIG.md` —— 桥的配置项全表
- `plugin/dapp/lightclient/rpc/lightclient/neutrino/BACKUP_RECOVERY.md` —— 备份/恢复
- `plugin/dapp/rgbx/cmd/ci/docker-compose.sh` —— 本环境的编排脚本
