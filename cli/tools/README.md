# 隐私股权发送工具

## 获取
```
$ git fetch git@github.com:bysomeone/plugin.git sendstocktool:sendstocktool
$ git checkout sendstocktool
```

## 编译
```
$ cd cli/tools && go build -o sendstock
```

## 配置文件
sendstock.toml
```toml
#chain33的客户端文件
cliCmd = "chain33-para"
#发送名单
csv = "test.csv"
#发送一笔交易后等待回执的超时时间，单位秒
checkTimeout = 30
#发送人地址
fromAddr = "12qyocayNF7Lv6C9qW4avxs2E7U41fKSfv"
#每笔交易发送间隔时间，单位ms
interval = 10
#CSV标题头行数
csvTitleLine = 1
#发送交易失败，最大重发时间,单位minute
reSendTimeout = 10

```

## csv格式
输入的csv有两列内容，用户id和具体的发送额度，test.csv示例
```csv
userID amout
111111 1
222222 2
```

## 运行
```
$ ./sendstock -f sendstock.toml
```

## 发送步骤

1. 搭建平行链，需要配置创世地址和额度
```toml
[consensus]
name="para"
genesisBlockTime=1514533394
genesis="14KEKbYtKKQm4wMthSK9J4La4nAiidGozt"

[consensus.sub.para]
genesisAmount=100000000
```
2. 解锁钱包，生成或者导入发送人的私钥，以12qyocayNF7Lv6C9qW4avxs2E7U41fKSfv为例
3. 基本转币操作
```
//从创世地址中转充足的coins到发送地址
$ chain33-para send coins transfer -t 12qyocayNF7Lv6C9qW4avxs2E7U41fKSfv -a 2000000 -k CC38546E9E659D15E6B4893F0AB32A06D103931A8230B0BDE71459D2B27D6944

//将发送人的coins转到对应的隐私合约中，额度大于发放股权总额度，合约名称根据需要替换成股权链
$ chain33-para send coins send_exec -e user.p.para.privacy -a 1000000 -k 12qyocayNF7Lv6C9qW4avxs2E7U41fKSfv

//开启钱包地址隐私交易功能
$ chain33-para privacy enable -a all
```
4. 使用发送工具发送股权，根据需要修改对应配置项，命令行，发送地址等
5. 等待发送完成，完成后将会把checking key保存于清单文件中，test.csv示例
```csv
userId,amount,reslut,checkingKey
111111,1,success,5f44f264aa3aedc3f304955760e19c7319ce4e7f2d1b3d22108424beb28600031b9f74be5da482a38d1d696a885ca168fcd012dbafcb595422192b2ae8c2fad1
222222,2,success,a98614fac852dadd925e111fd47df395f78ace36b12a91bd888777917d0baf0ff34ce72d7ccb2a9c29ab645918b1076c94e3887d38c6bf1bb36d3abcfab27830
```

## 校验工具
发送后可以用校验工具检测发送隐私股权额度的正确性，校验工具是调用股权查看接口模拟查看
### 获取
```
//https://gitlab.33.cn/jiangpeng/syncstockapi.git
$ git clone git@gitlab.33.cn:jiangpeng/syncstockapi.git $GOPATH/src/gitlab.33.cn/wallet/syncstockapi
```

### 编译
```
$ cd $GOPATH/src/gitlab.33.cn/wallet/syncstockapi/checktool
$ go build -o check
```

### 配置
check.toml
```toml
#CSV文件 发送工具执行后的清单，包含了checkingey信息
csv = "test.csv"

#csv文件标题行数
csvTitleLine = 1


#只检测大于该高度的资产，首次发放或者需要检测历史总份额时可以为0
checkFromHeight = 0


#检测单个用户超时，单位秒
checkTimeout = 20


#节点rpc地址，本地节点localhost
rpcAddr = "http://localhost:8901"
```
### 运行
```
$ ./check -f check.toml
```
会查看清单中所有人的额度是否正确，失败名单会保存在checkRes.csv文件中