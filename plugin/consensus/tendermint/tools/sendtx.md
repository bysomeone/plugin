### 发送交易使用说明

交易from地址：14KEKbYtKKQm4wMthSK9J4La4nAiidGozt
对应私钥：CC38546E9E659D15E6B4893F0AB32A06D103931A8230B0BDE71459D2B27D6944

发送交易需要签名操作，会占用部分cpu性能

#### 发送none交易


./sendtx none [grpc服务地址] [交易字节大小] [并发参数1] [并发参数2]

并发参数1：单次循环并发发送交易数
并发参数2：循环结束是否sleep 1s，0表示持续发送，大于0表示sleep 1s

./sendtx none localhost:8802 32 1000 0 // 持续发送none交易，交易字节大小为32
./sendtx none localhost:8802 32 1000 1 // 1s发送1000比交易

#### 发送coins交易

指定总数量的账户数，随机选取一个作为接收方地址，发送方地址恒定为挖矿地址14KEKbYtKKQm4wMthSK9J4La4nAiidGozt

./sendtx coins [grpc服务地址] [接收地址的账户数] [并发参数1] [并发参数2]

并发参数1：同上
并发参数2：同上

./sendtx coins localhost:8802 1000 1000 0   // 持续发送coins交易，交易接收账户总数为1000



### 配置说明


#### 不收取手续费

```toml
[mempool]
minTxFeeRate=0
maxTxFee=0
```

#### 收取手续费
收取手续费会降低性能，即扣取手续费在执行时涉及数据库读写

使用工具发送的交易手续费均为10，需要相应配置保证交易检查通过

```toml
[mempool]
minTxFeeRate=1
maxTxFeeRate=2
maxTxFee=100
```


#### 其他配置

性能测试，相关配置设置, 相关缓存大小根据内存调整

```toml
TxHeight=true
[log]
# 日志级别，支持debug(dbug)/info/warn/error(eror)/crit
loglevel = "info"
logConsoleLevel = "info"

[exec]
disableAddrIndex=true
```
