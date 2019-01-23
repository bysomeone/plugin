// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build go1.8

package main

import (
	_ "github.com/33cn/chain33/system"
	_ "github.com/33cn/plugin/plugin"

	autoTy  "github.com/33cn/chain33/cmd/autotest/types"

	"encoding/csv"
	"io/ioutil"
	"github.com/BurntSushi/toml"
	"strings"
	"github.com/33cn/plugin/cli/tools/common"

	"strconv"
	"time"
	"encoding/json"
	"os"
	"github.com/33cn/chain33/types"
	clog "github.com/33cn/chain33/common/log"
	log "github.com/33cn/chain33/common/log/log15"
	. "github.com/33cn/plugin/plugin/dapp/privacy/crypto"
	com "github.com/33cn/chain33/common"
	"io"
	"unsafe"
	"fmt"
)

type SendConfig struct {
	CliCommand      string         `toml:"cliCmd"`
	CsvPath         string         `toml:"csv"`
	CheckTimeout    int            `toml:"checkTimeout"`
	FromAddr        string         `toml:"fromAddr"`
	FromPri         string         `tom:"fromPri"`
	Interval        int            `toml:"interval"`
	CsvTitleLine    int            `toml:"csvTitleLine"`
}


type txResult struct {
	amount    string
	txHash    string
	result    bool
}

type rowData struct {
	userId       string
	totalAmount  string
	result       string
}

type rowDataResult struct {
	rd           *rowData
	txHashs      []*txResult
	priKey       *Privacy
}

func sendTx(cfg *SendConfig, userId string, privKey *[KeyLen32]byte, totalAmount string) (*rowDataResult, error) {
	// 由userId生成
	pri, err := common.NewPrivacyWithPrivKeyEx(privKey,[]byte(userId))
	if err != nil {
		log.Error("NewPrivacyWithPrivKeyEx fail", "userId", userId, "error", err)
		return nil, err
	}
	rd := &rowData{
		userId: userId,
		totalAmount: totalAmount,
	}
	rdr := &rowDataResult{
		rd:rd,
		priKey: pri,
	}
	amount, err := strconv.ParseInt(totalAmount, 10, 64)
	if err != nil {
		log.Error("str to int fail", "userId", userId, "error", err)
		return nil, err
	}
	var pair []byte
	pair = append(pair, pri.ViewPubkey[:]...)
	pair = append(pair, pri.SpendPubkey[:]...)
	keyPair := com.Bytes2Hex(pair)
	amount *= common.Coin
	amounts := common.DecomposeAmount2digits(amount, common.Coin)
	for _, a := range amounts {
		fa := float64(a/common.Coin)
		strAm := strconv.FormatFloat(fa, 'f', 6, 64)
		command := "send privacy pub2priv "
		command += " -a " + strAm
		command += " -k " + cfg.FromAddr
		command += " -p " + keyPair
		txHash, bSuccess := autoTy.SendTxCommand(command)
		if !bSuccess {
			log.Error("send tx fail", "userId", userId, "amount", strAm, "output", txHash)
		}
		tr := &txResult{amount:strAm, txHash: txHash,}
		rdr.txHashs = append(rdr.txHashs, tr)
		time.Sleep(time.Millisecond * time.Duration(cfg.Interval))
	}
	return rdr, nil
}

func checkTx(txhash string) bool {
	txInfo, ok := autoTy.GetTxInfo(txhash)
	if !ok {
		return false
	}
	var jsonMap map[string]interface{}
	err := json.Unmarshal([]byte(txInfo), &jsonMap)
	if err != nil {
		return false
	}
	_, ok = autoTy.GetTxRecpTyname(jsonMap)
	if !ok {
		return false
	}
	return true
}

func priToCheckKey(priKey *Privacy) string {
	var pair []byte
	pair = append(pair, priKey.ViewPrivKey[0:KeyLen32]...)
	pair = append(pair, priKey.SpendPubkey[:]...)
	return com.Bytes2Hex(pair)
}

func dumpKey(addr string) (string, error) {
	info, err := autoTy.RunChain33Cli(strings.Fields(fmt.Sprintf("account dump_key -a %s", addr)))
	if err != nil {
		return "", err
	}
	var jsonMap map[string]interface{}
	err = json.Unmarshal([]byte(info), &jsonMap)
	if err != nil {
		return "", err
	}
	if _, ok := jsonMap["data"]; !ok {
		return "", err
	}
	return jsonMap["data"].(string), nil
}

func main() {
	clog.SetFileLog(&types.Log{
		Loglevel:        "debug",
		LogConsoleLevel: "info",
		LogFile:         "logs/log.log",
		MaxFileSize:     400,
		MaxBackups:      100,
		MaxAge:          28,
		LocalTime:       true,
		Compress:        false,
	})
	var cfg SendConfig
	if _, err := toml.DecodeFile("send.toml", &cfg); err != nil {
		log.Error("DecodeFile toml fail", "error", err)
		return
	}
	if cfg.CsvTitleLine < 0 {
		log.Error("Set csv title line fail")
		return
	}
	autoTy.CliCmd = cfg.CliCommand

	//


	dat, err := ioutil.ReadFile(cfg.CsvPath)
	if err != nil {
		log.Error("Read csv file fail", "error", err)
		return
	}
	r := csv.NewReader(strings.NewReader(string(dat[:])))
	var rds []*rowData
	rowNum := 0
	//读取数据
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(record) < 2 {
			log.Error("Read csv file fail", "error", err, "csv file column number", len(record), "rowNum", rowNum)
			return
		}
		result := ""
		if rowNum < cfg.CsvTitleLine {
			result = "result"
			if len(record) > 2 {
				result = record[2]
			}
		}
		rd := &rowData{
			userId:record[0],
			totalAmount: record[1],
			result: result,
		}
		rds = append(rds, rd)
		rowNum++
	}

	StartCheck(cfg.CsvPath, rowNum - cfg.CsvTitleLine)
	key, err := dumpKey(cfg.FromAddr)
	if err != nil {
		log.Info("dumpKey fail", "error", err)
		return
	}
	cfg.FromPri = key

	//生成发送人的Privacy
	prib, err := com.FromHex(cfg.FromPri)
	if err != nil {
		log.Info("FromPri to byte fail", "error", err)
		return
	}
	pri, err := NewPrivacyWithPrivKey((*[KeyLen32]byte)(unsafe.Pointer(&prib[0])))
	if err != nil {
		log.Info("NewPrivacyWithPrivKey fail", "error", err)
		return
	}

	//发送数据
	var rdrs []*rowDataResult
	for _, rd := range rds[cfg.CsvTitleLine:] {
		viewP := pri.ViewPrivKey[0:KeyLen32]
		log.Info("SendStockToUser", "uid", rd.userId, "SendAmount", rd.totalAmount)
		rdr, err := sendTx(&cfg, rd.userId, (*[KeyLen32]byte)(unsafe.Pointer(&viewP[0])), rd.totalAmount)
		if err != nil {
			log.Error("Send tx group fail", "error", err)
			rdr.rd.result = "fail"
		}
		rdrs = append(rdrs, rdr)
	}

	//休眠
	if cfg.CheckTimeout < 16 {
		cfg.CheckTimeout = 16
	}
	//检查交易
	isAllSucc := false
	for k := 0; k < cfg.CheckTimeout; k++ {
		isAllSucc = true
		for i := 0; i < len(rdrs); i++ {
			rdrs[i].rd.result = "success"
			for j := 0; j < len(rdrs[i].txHashs); j++ {
				ok := checkTx(rdrs[i].txHashs[j].txHash)
				if !ok {
					rdrs[i].txHashs[j].result = false
					rdrs[i].rd.result = "fail"
					isAllSucc = false
				} else {
					rdrs[i].txHashs[j].result = true
				}
				log.Debug("tx query", "userId", rdrs[i].rd.userId, "amount", rdrs[i].txHashs[j].amount,
					"tx hash", rdrs[i].txHashs[j].txHash, "result", rdrs[i].txHashs[j].result)
			}
		}
		if isAllSucc {
			break
		}
		time.Sleep(time.Second)
	}

	//更新发送结果到csv
	file, _ := os.OpenFile(cfg.CsvPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.ModePerm)
	w := csv.NewWriter(file)
	//写表头
	for _, r := range rds[:cfg.CsvTitleLine] {
		w.Write([]string{r.userId, r.totalAmount, r.result, "checkingKey"})
		w.Flush()
	}
	for _, r := range rdrs {
		keyPair := priToCheckKey(r.priKey)
		w.Write([]string{r.rd.userId, r.rd.totalAmount, r.rd.result, keyPair})
		w.Flush()
		amount, _ := strconv.ParseInt(r.rd.totalAmount, 10, 64)
		SendToCheck(r.rd.userId, keyPair, amount)
	}
	file.Close()

	//保存失败的发送结果
	if !isAllSucc {
		log.Error("send some tx fail!")
		path := cfg.CsvPath[:len(cfg.CsvPath)-len(".csv")] + "_resend" + ".csv"
		file, _ := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.ModePerm)
		w := csv.NewWriter(file)
		//写表头
		for _, r := range rds[:cfg.CsvTitleLine] {
			w.Write([]string{r.userId, r.totalAmount, r.result, "a+B", "txhash"})
			w.Flush()
		}
		for _, rdr := range rdrs {
			if rdr.rd.result == "fail" {
				keyPair := priToCheckKey(rdr.priKey)
				for _, txr := range rdr.txHashs {
					if !txr.result {
						w.Write([]string{rdr.rd.userId, txr.amount, "fail", keyPair, txr.txHash})
						w.Flush()
					}
				}
			}
		}
		file.Close()
	} else {
		log.Info("send all tx success!")
		os.RemoveAll(cfg.CsvPath[:len(cfg.CsvPath)-len(".csv")] + "_resend" + ".csv")
	}

	WaitCheck()
}
