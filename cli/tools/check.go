package main

import (
	"encoding/csv"
	"fmt"
	"github.com/inconshreveable/log15"
	"gitlab.33.cn/wallet/syncstockapi"
	"math/rand"
	"os"
	"sync"
	"time"
)

var (

	checkChan = make (chan *checkInfo, 1000)
	checkExpire = time.Minute
	checkLog = log15.New()

	wg = &sync.WaitGroup{}
	generateNum = 200
)

func StartCheck(filename string, checkNum int) {

	//generateUid(filename, generateNum)

	wg.Add(1)
	go CheckStock(checkNum)

}


type checkInfo struct {

	uid string
	key string
	amount int64
}



func SendToCheck(uid, key string, amount int64) {

	checkChan <- &checkInfo{
		uid:uid,
		key:key,
		amount:amount,
	}
}


func WaitCheck() {
	wg.Wait()
}



func CheckStock(checkNum int) {

	err := syncstockapi.SyncStockInit("http://localhost:8901", "./stock.db")

	if err != nil {
		panic("checkStock")
	}
	defer wg.Done()
	defer syncstockapi.SyncStockFree()
	recvNum := 0

	for {


		select {

		case info := <- checkChan:
			recvNum++
			checkBalance(info)
			if recvNum >= checkNum{
				return
			}

		}
	}
}


func checkBalance(info *checkInfo) {


	prevBalance := syncstockapi.GetPrivacyStockBalance(info.key)
	expectBalance := prevBalance + info.amount
	currBalance := prevBalance

	err := syncstockapi.StartSyncingPrivacyStock(info.key)

	timer := time.NewTimer(checkExpire)
	bSuccess := true
	keepLoop := true

	if err != nil {
		keepLoop = false
		bSuccess = false
	}


	for keepLoop {

		select {

		case <-timer.C:
			bSuccess = false
			keepLoop = false
			err = fmt.Errorf("CheckExpire")
		default:
			currBalance = syncstockapi.GetPrivacyStockBalance(info.key)
			if currBalance == expectBalance {
				keepLoop = false
			}else {
				time.Sleep(time.Second)
			}
		}
	}

	timer.Stop()
	syncstockapi.StopSyncingPrivacyStock()

	if bSuccess {
		checkLog.Info("CheckStockResult", "uid", info.uid, "prevBalance", prevBalance, "currBalance", expectBalance, "SendAmount", info.amount, "Result", "Succeed")
	}else {
		checkLog.Error("CheckStockResult", "uid", info.uid, "prevBalance", prevBalance, "currBalance", expectBalance, "SendAmount", info.amount, "Err", err.Error())
	}
}


func generateUid(filename string, num int) {

	uidMap := make(map[int]int, num)

	gnum := 0
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for gnum < num {

		id := int(r.Int63() % 99999) + 1

		if _,ok := uidMap[id]; ok {
			continue
		}
		//100 倍数，小于100000
		amount := int(((r.Int63() % 10000 + 1) / 100) * 100)
		uidMap[id] = amount
		gnum ++
	}

	//更新发送结果到csv
	file, _ := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.ModePerm)
	w := csv.NewWriter(file)
	//写表头
	_ = w.Write([]string{"uid", "amount"})
	w.Flush()

	for uid, amount := range uidMap {
		_ = w.Write([]string{fmt.Sprintf("%05d", uid), fmt.Sprintf("%d", amount)})
		w.Flush()
	}

	_=file.Close()

}