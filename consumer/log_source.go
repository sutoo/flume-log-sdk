package consumer

import (
	"container/list"
	"encoding/json"
	"flume-log-sdk/config"
	"flume-log-sdk/consumer/client"
	"flume-log-sdk/consumer/pool"
	"flume-log-sdk/rpc/flume"
	"fmt"
	"log"
	"math/rand"
	"sync/atomic"
	"time"
)

type counter struct {
	lastSuccValue int64

	currSuccValue int64

	lastFailValue int64

	currFailValue int64
}

// 用于向flume中作为sink 通过thrift客户端写入日志

type SourceServer struct {
	flumeClientPool *list.List
	isStop          bool
	monitorCount    counter
	business        string
	batchSize       int
	buffChannel     chan *flume.ThriftFlumeEvent
}

func newSourceServer(business string, flumePool *list.List) (server *SourceServer) {
	batchSize := 300
	sendbuff := 500
	buffChannel := make(chan *flume.ThriftFlumeEvent, sendbuff)
	sourceServer := &SourceServer{
		business:        business,
		flumeClientPool: flumePool,
		batchSize:       batchSize,
		buffChannel:     buffChannel}
	return sourceServer
}

func (self *SourceServer) monitor() (succ, fail int64, bufferSize int) {
	currSucc := self.monitorCount.currSuccValue
	currFail := self.monitorCount.currFailValue
	succ = (currSucc - self.monitorCount.lastSuccValue)
	fail = (currFail - self.monitorCount.lastFailValue)
	self.monitorCount.lastSuccValue = currSucc
	self.monitorCount.lastFailValue = currFail

	//自己的Buffer大小
	bufferSize = len(self.buffChannel)
	return
}

//启动pop
func (self *SourceServer) start() {

	self.isStop = false

	//创建chan ,buffer 为10
	sendbuff := make(chan []*flume.ThriftFlumeEvent, self.batchSize)
	//启动20个go程从channel获取
	for i := 0; i < 10; i++ {
		go func(ch chan []*flume.ThriftFlumeEvent) {
			for !self.isStop {
				events := <-ch
				self.innerSend(events)
			}
		}(sendbuff)
	}

	go func() {
		//批量收集数据
		pack := make([]*flume.ThriftFlumeEvent, 0, self.batchSize)
		lastCheckTime := time.Now().Unix()
		for !self.isStop {
			event := <-self.buffChannel
			//如果总数大于batchsize则提交
			if len(pack) < self.batchSize && time.Now().Unix()-lastCheckTime <= 0 {
				//批量提交
				pack = append(pack, event)
				continue
			}
			lastCheckTime = time.Now().Unix()
			sendbuff <- pack[:len(pack)]
			pack = make([]*flume.ThriftFlumeEvent, 0, self.batchSize)
		}

		close(sendbuff)
	}()

	log.Printf("LOG_SOURCE|SOURCE SERVER [%s]|STARTED\n", self.business)
}

func (self *SourceServer) innerSend(events []*flume.ThriftFlumeEvent) {

	for i := 0; i < 3; i++ {
		pool := self.getFlumeClientPool()
		flumeclient, err := pool.Get(5 * time.Second)
		if nil != err || nil == flumeclient {
			log.Printf("LOG_SOURCE|GET FLUMECLIENT|FAIL|%s|%s|TRY:%d\n", self.business, err, i)
			continue
		}

		err = flumeclient.AppendBatch(events)
		defer func() {
			if err := recover(); nil != err {
				//回收这个坏的连接
				pool.ReleaseBroken(flumeclient)
			} else {
				pool.Release(flumeclient)
			}
		}()

		if nil != err {
			atomic.AddInt64(&self.monitorCount.currFailValue, int64(1*self.batchSize))
			log.Printf("LOG_SOURCE|SEND FLUME|FAIL|%s|%s|TRY:%d\n", self.business, err.Error(), i)

		} else {
			atomic.AddInt64(&self.monitorCount.currSuccValue, int64(1*self.batchSize))
			if rand.Int()%10000 == 0 {
				log.Printf("trace|send 2 flume succ|%s|%d\n", flumeclient.HostPort(), len(events))
			}
			break
		}

	}
}

//解析出decodecommand
func decodeCommand(resp []byte) (string, *flume.ThriftFlumeEvent) {
	var cmd config.Command
	err := json.Unmarshal(resp, &cmd)
	if nil != err {
		log.Printf("command unmarshal fail ! %T | error:%s\n", resp, err.Error())
		return "", nil
	}
	//
	momoid := cmd.Params["momoid"].(string)

	businessName := cmd.Params["businessName"].(string)

	action := cmd.Params["type"].(string)

	bodyContent := cmd.Params["body"]

	//将businessName 加入到body中
	bodyMap := bodyContent.(map[string]interface{})
	bodyMap["business_type"] = businessName

	body, err := json.Marshal(bodyContent)
	if nil != err {
		log.Printf("marshal log body fail %s", err.Error())
		return businessName, nil
	}

	//拼Body
	flumeBody := fmt.Sprintf("%s\t%s\t%s", momoid, action, string(body))
	event := client.NewFlumeEvent(businessName, action, []byte(flumeBody))
	return businessName, event
}

func (self *SourceServer) stop() {
	self.isStop = true
	time.Sleep(5 * time.Second)

	//遍历所有的flumeclientlink，将当前Business从该链表中移除
	for v := self.flumeClientPool.Back(); nil != v; v = v.Prev() {
		v.Value.(*pool.FlumePoolLink).DetachBusiness(self.business)
	}
	close(self.buffChannel)
	log.Printf("LOG_SOURCE|SOURCE SERVER|[%s]|STOPPED\n", self.business)
}

func (self *SourceServer) getFlumeClientPool() *pool.FlumeClientPool {

	//采用轮训算法
	e := self.flumeClientPool.Back()
	self.flumeClientPool.MoveToFront(e)
	return e.Value.(*pool.FlumePoolLink).FlumePool

}
