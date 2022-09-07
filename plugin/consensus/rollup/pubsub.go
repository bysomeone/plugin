package rollup

import (
	"time"

	"github.com/33cn/chain33/queue"
	"github.com/33cn/chain33/system/consensus"
	"github.com/33cn/chain33/types"
	rolluptypes "github.com/33cn/plugin/plugin/dapp/rollup/types"
)

const (
	psValidatorSignTopic = "rollup/valSignMsg/1.0"
)

func (r *RollUp) SubMsg(msg *queue.Message) {

	data, ok := msg.Data.(*types.TopicData)
	if msg.Ty != types.EventReceiveSubData || !ok {

		return
	}
	rlog.Debug("subMsg", "topic", data.Topic, "from", data.From)
	if data.Topic != psValidatorSignTopic {
		return
	}
	signMsg := &rolluptypes.ValidatorSignMsg{}
	err := types.Decode(data.GetData(), signMsg)
	if err != nil {
		rlog.Error("decode", "topic", data.Topic, "err", err)
		return
	}

	r.subChan <- signMsg
}

func (r *RollUp) trySubTopic(topic string) {

	data := &types.SubTopic{Topic: topic, Module: consensus.ModuleName}

	for {
		err := r.sendP2PMsg(types.EventSubTopic, data)
		if err == nil {
			break
		}
		rlog.Debug("trySubTopic", "err", err)
		time.Sleep(time.Second)
	}
}

func (r *RollUp) tryPubMsg(topic string, msg []byte) {

	data := &types.PublishTopicMsg{Topic: topic, Msg: msg}
	tryCount := 0
	for {
		tryCount++
		err := r.sendP2PMsg(types.EventPubTopicMsg, data)
		if err == nil || tryCount >= 3 {
			break
		}
		rlog.Error("tryPubMsg", "topic", topic, "err", err)
		time.Sleep(time.Second)
	}
}

func (r *RollUp) handleSubMsg() {

	for {

	}
}