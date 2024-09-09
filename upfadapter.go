// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/omec-project/upfadapter/config"
	"github.com/omec-project/upfadapter/logger"
	"github.com/omec-project/upfadapter/metrics"
	"github.com/omec-project/upfadapter/pfcp"
	"github.com/omec-project/upfadapter/pfcp/udp"
	"github.com/wmnsk/go-pfcp/message"
)

// Hnadler for SMF initiated msgs
func handler(w http.ResponseWriter, req *http.Request) {
	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		logger.AppLog.Errorf("server: could not read request body: %s\n", err)
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

	logger.AppLog.Debugf("\n received msg type [%v], upf nodeId [%s], smfIp [%v], msg [%v]",
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

	// Initialise Statistics
	go metrics.InitMetrics()

	http.HandleFunc("/", handler)
	err := http.ListenAndServe(":8090", nil)
	if err != nil {
		logger.AppLog.Errorf("error listening TCP connection: %v", err)
	}
}
