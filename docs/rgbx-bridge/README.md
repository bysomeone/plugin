# RGB 桥交付文档

chain33 `rgbx` 跨链桥（XBTC / RGB20 USDT）的交付文档集。三份文档分别面向不同读者：

| 文档 | 读者 | 内容 |
| --- | --- | --- |
| [01-系统设计.md](01-系统设计.md) | 架构评审、安全审计、开发 | 信任模型、组件、充值/提现流程、安全机制、失败分类、测试覆盖边界 |
| [02-运维手册.md](02-运维手册.md) | 运维、SRE | 部署拓扑、上线清单、子文档索引 |
| [03-前端对接.md](03-前端对接.md) | 钱包 / 前端开发 | 充值请求签名、invoice、提现、查询接口、金额与手续费口径、联调清单 |

运维手册的子文档：

- [02-1-部署与配置.md](02-1-部署与配置.md)：部署步骤、生产必须值、网关运维
- [02-2-备份与恢复.md](02-2-备份与恢复.md)：备份范围、冷备/恢复命令、演练覆盖边界
- [02-3-监控与告警.md](02-3-监控与告警.md)：健康检查、日志关键字、事件账本、指标与告警
- [02-4-应急预案.md](02-4-应急预案.md)：按场景的处置表
- [02-5-治理与日常操作.md](02-5-治理与日常操作.md)：治理提案/审批、暂停、手续费、加价、放弃、急停、熔断

配置项的逐项权威说明在
[CONFIG.md](../../plugin/dapp/lightclient/rpc/lightclient/neutrino/CONFIG.md)；备份设计细节在
[BACKUP_RECOVERY.md](../../plugin/dapp/lightclient/rpc/lightclient/neutrino/BACKUP_RECOVERY.md)；
提现锁定/解锁模型在
[WITHDRAW_LOCK_MODEL.md](../../plugin/dapp/lightclient/rpc/lightclient/neutrino/WITHDRAW_LOCK_MODEL.md)。

## 文档一致性

gateway 之前架构的旧文档（TSS 地址 + OP_RETURN 充值、RGB Proxy、旧的 `[neutrino.RGB20]` 配置字段）
已删除，包括 `FUNCTIONAL.md`、`TECHNICAL.md`、`TECHNICAL1.md`、`WHITEPAPER.md`、
`RGB_USDT_INTEGRATION.md`、`RGB_USDT_PRODUCT.md`、`LAUNCH_READINESS.md`。

仍具参考价值：`PERF_P2WSH.md`（性能口径与基准）、`RGB_USDT_ONBOARDING.md`（对外接入需求单）、
`BACKUP_RECOVERY.md`（备份设计与缺口清单）、`WITHDRAW_LOCK_MODEL.md`（解锁模型）、
网关 `README.md`（协议 0.2 与事件账本）。
