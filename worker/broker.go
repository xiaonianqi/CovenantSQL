/*
 * Copyright 2019 The CovenantSQL Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/CovenantSQL/CovenantSQL/conf"
	"github.com/CovenantSQL/CovenantSQL/proto"
	"github.com/CovenantSQL/CovenantSQL/types"
	"github.com/CovenantSQL/CovenantSQL/utils/log"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/gogf/gf/g/container/gqueue"
)

type MQTTAPI string

const (
	//Publish API
	MQTTNewest MQTTAPI = "newest"
	DSNList    MQTTAPI = "dsnlist"

	//Subscribe API
	MQTTWrite  MQTTAPI = "write"
	MQTTReplay MQTTAPI = "replay"
	MQTTCreate MQTTAPI = "create"

	MQTTInvalid MQTTAPI = ""
)

var (
	publishPrefix = "/cql/miner/"
	listenPrefix  = "/cql/client/"
)

type SubscribeEvent struct {
	ClientID   proto.NodeID
	DatabaseID proto.DatabaseID
	ApiName    MQTTAPI
	Payload    BrokerPayload
}

type BrokerPayload struct {
	BlockID        int32         `json:"block_id"`
	BlockIndex     int           `json:"block_index"`
	ClientID       proto.NodeID  `json:"client_id"`
	ClientSequence uint64        `json:"client_seq"`
	Events         []types.Query `json:"events"`

	//Replay API
	BlockStart int32 `json:"block_start"`
	IndexStart int   `json:"index_start"`
	BlockEnd   int32 `json:"block_end"`
	IndexEnd   int   `json:"index_end"`
}

type MQTTClient struct {
	mqtt.Client
	ListenTopic        string
	PublishTopicPrefix string

	subscribeEventQueue *gqueue.Queue

	updateCtx    context.Context
	updateCancel context.CancelFunc

	dbms *DBMS
}

func NewMQTTClient(config *conf.MQTTBrokerInfo, dbms *DBMS) (c *MQTTClient) {
	if config == nil {
		return
	}

	opts := mqtt.NewClientOptions()
	opts.AddBroker(config.Addr)
	opts.SetUsername(config.User)
	opts.SetPassword(config.Password)
	opts.SetClientID(config.User)
	opts.SetOrderMatters(true)

	c = &MQTTClient{
		Client:              mqtt.NewClient(opts),
		ListenTopic:         listenPrefix + "#",
		PublishTopicPrefix:  publishPrefix + config.User + "/",
		subscribeEventQueue: gqueue.New(),
		dbms:                dbms,
	}
	if token := c.Connect(); token.Wait() && token.Error() != nil {
		log.Errorf("Connect broker failed: %v", token.Error())
		return nil
	}

	c.updateCtx, c.updateCancel = context.WithCancel(context.Background())
	go c.updateBlockLoop()

	go c.subscribeEventLoop()
	c.Subscribe(c.ListenTopic, 1, subscribeCallback(c.subscribeEventQueue))

	return
}

func decodeTopicAPI(topic string) (clientID proto.NodeID, databaseID proto.DatabaseID, apiName MQTTAPI) {
	args := strings.Split(strings.TrimPrefix(topic, listenPrefix), "/")
	if len(args) == 0 || len(args) > 3 {
		return
	}
	switch len(args) {
	case 1:
		clientID = proto.NodeID(args[0])
	case 2:
		clientID = proto.NodeID(args[0])
		if args[1] == string(MQTTCreate) {
			apiName = MQTTCreate
		} else {
			databaseID = proto.DatabaseID(args[1])
		}
	case 3:
		clientID = proto.NodeID(args[0])
		databaseID = proto.DatabaseID(args[1])
		apiName = MQTTAPI(args[2])
	default:
		return
	}

	// valid check
	if apiName != MQTTWrite && apiName != MQTTReplay && apiName != MQTTCreate {
		apiName = MQTTInvalid
	}
	return
}

func subscribeCallback(eventQueue *gqueue.Queue) func(client mqtt.Client, msg mqtt.Message) {
	return func(client mqtt.Client, msg mqtt.Message) {
		log.Debugf("TOPIC: %s\n", msg.Topic())
		clientID, databaseID, apiName := decodeTopicAPI(msg.Topic())
		if apiName == "" {
			log.Errorf("Invalid Topic: %s, Payload: %s\n", msg.Topic(), msg.Payload())
			return
		}

		var payload BrokerPayload
		err := json.Unmarshal(msg.Payload(), &payload)
		if err != nil {
			log.Errorf("Invalid MSG: %s, err: %v\n", msg.Payload(), err)
			return
		}
		log.Debugf("Payload: %v\n", payload)
		eventQueue.Push(&SubscribeEvent{
			ApiName:    apiName,
			ClientID:   clientID,
			DatabaseID: databaseID,
			Payload:    payload,
		})
	}
}

func (c *MQTTClient) subscribeEventLoop() {
	for raw := range c.subscribeEventQueue.C {
		subscribeEvent := raw.(*SubscribeEvent)
		switch subscribeEvent.ApiName {
		case MQTTWrite:
			c.processWriteEvent(subscribeEvent)
		case MQTTReplay:
			c.processReplayEvent(subscribeEvent)
		case MQTTCreate:
			c.processCreateEvent(subscribeEvent)
		default:
			log.Errorf("Unknow API name %s\n", subscribeEvent.ApiName)
		}
	}
}

// TODO
// 3. reconsider context
func (c *MQTTClient) processWriteEvent(event *SubscribeEvent) {
	log.Debugf("Processed write event: %s %s %s\n", event.ClientID, event.DatabaseID, event.ApiName)

	// 1. add to sqlchain
	var db *Database
	var exists bool
	// find database
	if db, exists = c.dbms.getMeta(proto.DatabaseID(event.DatabaseID)); !exists {
		log.Errorf("MQTT write database not exist: %v", event)
		return
	}

	// 2. build request
	req := &types.Request{
		Header: types.SignedRequestHeader{
			RequestHeader: types.RequestHeader{
				QueryType:    types.WriteQuery,
				NodeID:       proto.NodeID(event.ClientID),
				DatabaseID:   proto.DatabaseID(event.DatabaseID),
				ConnectionID: 0,
				SeqNo:        event.Payload.ClientSequence,
				Timestamp:    getLocalTime(),
			},
		},
		Payload: types.RequestPayload{
			Queries: event.Payload.Events,
		},
	}

	_, err := db.Query(req)
	if err != nil {
		log.Errorf("MQTT write database failed: %v, err:%v", event, err)
		return
	}
	// TODO
	// 3. make it unblock
}

func (c *MQTTClient) processReplayEvent(event *SubscribeEvent) {
	log.Debugf("Processed replay event: %s %s %s\n", event.ClientID, event.DatabaseID, event.ApiName)
	// TODO
	// make it unblock

	dbID := event.DatabaseID
	rawDB, ok := c.dbms.dbMap.Load(dbID)
	if !ok {
		log.Errorf("MQTT fetch block failed, databaseID not exist: %v", dbID)
		return
	}
	db := rawDB.(*Database)

	bStart := event.Payload.BlockStart
	bEnd := event.Payload.BlockEnd
	iStart := event.Payload.IndexStart
	iEnd := event.Payload.IndexEnd

	for blockIndex := bStart; blockIndex <= bEnd; blockIndex++ {
		block, realCount, _, err := db.chain.FetchBlockByCount(-1)
		if err != nil {
			log.Errorf("MQTT fetch block failed, databaseID: %v, err: %v", dbID, err)
			return
		}

		for index, qat := range block.QueryTxs {
			payload := BrokerPayload{
				BlockID:        realCount,
				BlockIndex:     index,
				ClientID:       qat.Request.Header.NodeID,
				ClientSequence: qat.Request.Header.SeqNo,
				Events:         qat.Request.Payload.Queries,
			}
			if blockIndex == bStart {
				if index < iStart {
					continue
				}
			} else if blockIndex == bEnd {
				if index > iEnd {
					// Success publish all replay
					return
				}
			}
			err = c.PublishDSN(MQTTReplay, qat.Request.Header.DatabaseID, payload, payload.ClientID)
			if err != nil {
				log.Errorf("MQTT publish replay api failed, databaseID: %v, payload: %v, err: %v", qat.Request.Header.DatabaseID, payload, err)
				// Cancel further publish if any error
				return
			}
		}
	}
}

func (c *MQTTClient) processCreateEvent(event *SubscribeEvent) {
	log.Debugf("Create API does not support yet.\n")
}

func (c *MQTTClient) updateBlockLoop() {
	for {
		select {
		case <-c.updateCtx.Done():
			return
		case <-time.After(conf.GConf.SQLChainPeriod):
			// TODO
			// make it unblock
			c.dbms.dbMap.Range(func(_, rawDB interface{}) bool {
				db := rawDB.(*Database)
				block, realCount, _, err := db.chain.FetchBlockByCount(-1)
				if err != nil {
					log.Errorf("MQTT fetch block failed: databaseID: %v, err: %v", db.dbID, err)
					return false
				}

				err = nil
				for index, qat := range block.QueryTxs {
					payload := BrokerPayload{
						BlockID:        realCount,
						BlockIndex:     index,
						ClientID:       qat.Request.Header.NodeID,
						ClientSequence: qat.Request.Header.SeqNo,
						Events:         qat.Request.Payload.Queries,
					}
					err = c.PublishDSN(MQTTNewest, qat.Request.Header.DatabaseID, payload, payload.ClientID)
					if err != nil {
						log.Errorf("MQTT publish newest api failed, databaseID: %v, payload: %v, err: %v", qat.Request.Header.DatabaseID, payload, err)
					}
				}
				if err != nil {
					return false
				}
				return true
			})

		}
	}
}

func (c *MQTTClient) PublishDSN(apiName MQTTAPI, databaseID proto.DatabaseID, payload BrokerPayload, requestClient proto.NodeID) error {
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	var topic string

	switch apiName {
	case MQTTNewest:
		topic = c.PublishTopicPrefix + string(databaseID) + "/" + string(apiName)
	case MQTTReplay:
		topic = c.PublishTopicPrefix + string(databaseID) + "/" + string(apiName) + "/" + string(requestClient)
	default:
		return errors.New("Invalid miner push api name" + string(apiName))
	}

	token := c.Publish(topic, 1, true, jsonBytes)
	if !token.Wait() {
		return token.Error()
	}
	return nil
}

func (c *MQTTClient) Close() {
	c.updateCancel()
	c.Unsubscribe(c.ListenTopic).Wait()
	c.subscribeEventQueue.Close()
	c.Disconnect(250)
}