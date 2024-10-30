// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"encoding/json"
	"io"
	"net/http"
    "flag"

	"github.com/omec-project/upfadapter/config"
	"github.com/omec-project/upfadapter/logger"
	"github.com/omec-project/upfadapter/pfcp"
	"github.com/omec-project/upfadapter/pfcp/udp"
	"github.com/wmnsk/go-pfcp/message"
	"github.com/omec-project/upfadapter/metrics"
	"github.com/omec-project/upfadapter/factory"
)

// Hnadler for SMF initiated msgs
func handler(w http.ResponseWriter, req *http.Request) {
	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		logger.AppLog.Errorf("server: could not read request body: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	var udpPodMsg config.UdpPodPfcpMsg
	err = json.Unmarshal(reqBody, &udpPodMsg)
	if err != nil {
		logger.AppLog.Errorf("error unmarshalling pfcp msg")
		return
	}

	pfcpMessage, err := message.Parse(udpPodMsg.Msg.Body)
	if err != nil {
		logger.AppLog.Errorf("error parsing pfcp msg")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	logger.AppLog.Debugf("received msg type [%v], upf nodeId [%s], smfIp [%v], msg [%v]",
		pfcpMessage.MessageType(), udpPodMsg.UpNodeID.NodeIdValue, udpPodMsg.SmfIp, udpPodMsg.Msg)

	pfcpJsonRsp, err := pfcp.ForwardPfcpMsgToUpf(pfcpMessage, udpPodMsg.UpNodeID)
	if err != nil {
		logger.AppLog.Errorf("error forwarding pfcp msg to UPF: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(pfcpJsonRsp)
	if err != nil {
		logger.AppLog.Errorf("error writing pfcp msg: %v", err)
	}
	logger.AppLog.Debugf("response sent for %v", pfcpMessage.MessageType())
}

// UDP handler for pfcp msg from UPF
func init() {
	go udp.Run(pfcp.Dispatch)
}

// Handler for msgs from SMF
func main() {

    // Read provided config
    cfgFilePtr := flag.String("config", "../../config/config.yaml", "is a config file")
    flag.Parse()
    logger.AppLog.Infof("UPF adapter has started with configuration file [%v]", *cfgFilePtr)

    factory.InitConfigFactory(*cfgFilePtr)

	// Initialise Statistics
	go metrics.InitMetrics()

	// Init Kafka stream
	if err := metrics.InitialiseKafkaStream(factory.UpfAdapterConfig.Configuration); err != nil {
		logger.KafkaLog.Errorf("initialise kafka stream failed, %v ", err.Error())
	}

	http.HandleFunc("/", handler)
	err := http.ListenAndServe(":8090", nil)
	if err != nil {
		logger.AppLog.Errorf("error listening TCP connection: %v", err)
	}
}
