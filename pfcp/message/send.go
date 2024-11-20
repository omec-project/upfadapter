// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
package message

import (
	"net"
	"time"

	"github.com/omec-project/upfadapter/config"
	"github.com/omec-project/upfadapter/logger"
	"github.com/omec-project/upfadapter/pfcp/handler"
	"github.com/omec-project/upfadapter/pfcp/udp"
	"github.com/omec-project/upfadapter/types"
	"github.com/wmnsk/go-pfcp/message"
)

func SendPfcpAssociationSetupRequest(upNodeID types.NodeID, pMsg message.Message) error {
	logger.PfcpLog.Debugf("send pfcp association request to upfNodeId [%v], pfcpMsg [%v]", upNodeID, pMsg)
	addr := &net.UDPAddr{
		IP:   upNodeID.ResolveNodeIdToIp(),
		Port: udp.PFCP_PORT,
	}
	eventData := udp.PfcpEventData{LSEID: 0, ErrHandler: handler.HandlePfcpSendError}
	logger.PfcpLog.Debugf("send pfcp msg addr [%v], pfcpMsg [%v]", addr, pMsg)
	if err := udp.SendPfcp(pMsg, addr, eventData); err != nil {
		return err
	}
	return nil
}

func SendHeartbeatRequest(upNodeID types.NodeID, pMsg message.Message) error {
	addr := &net.UDPAddr{
		IP:   upNodeID.ResolveNodeIdToIp(),
		Port: udp.PFCP_PORT,
	}
	err := udp.SendPfcp(pMsg, addr, nil)
	if err != nil {
		logger.PfcpLog.Errorf("failed to send heartbeat request for upf [%v]", upNodeID)
		return err
	}
	return nil
}

func SendPfcpSessionEstablishmentRequest(upNodeID types.NodeID, pMsg message.Message) error {
	upaddr := &net.UDPAddr{
		IP:   upNodeID.ResolveNodeIdToIp(),
		Port: udp.PFCP_PORT,
	}
	eventData := udp.PfcpEventData{LSEID: 0, ErrHandler: handler.HandlePfcpSendError}
	if err := udp.SendPfcp(pMsg, upaddr, eventData); err != nil {
		return err
	}
	return nil
}

func SendPfcpSessionModificationRequest(upNodeID types.NodeID, pMsg message.Message) error {
	upaddr := &net.UDPAddr{
		IP:   upNodeID.ResolveNodeIdToIp(),
		Port: udp.PFCP_PORT,
	}
	eventData := udp.PfcpEventData{LSEID: 0, ErrHandler: handler.HandlePfcpSendError}
	if err := udp.SendPfcp(pMsg, upaddr, eventData); err != nil {
		return err
	}
	return nil
}

func SendPfcpSessionDeletionRequest(upNodeID types.NodeID, pMsg message.Message) error {
	upaddr := &net.UDPAddr{
		IP:   upNodeID.ResolveNodeIdToIp(),
		Port: udp.PFCP_PORT,
	}
	eventData := udp.PfcpEventData{LSEID: 0, ErrHandler: handler.HandlePfcpSendError}
	if err := udp.SendPfcp(pMsg, upaddr, eventData); err != nil {
		return err
	}
	return nil
}

// Go routine to send hearbeat towards UPFs
func ProbeUpfHearbeatReq() {
	for {
		time.Sleep(5 * time.Second)
		for nodeId, upf := range config.UpfCfg.UPFs {
			logger.PfcpLog.Debugf("sending heartbeat request to upf [%v]", nodeId)
			if config.IsUpfAssociated(upf.NodeID) {
				pfcpMsg, err := config.BuildPfcpHeartbeatRequest()
				if err != nil {
					logger.PfcpLog.Errorf("failed to build heartbeat request for upf [%v]", nodeId)
					continue
				}
				err1 := SendHeartbeatRequest(upf.NodeID, pfcpMsg)
				if err1 != nil {
					continue
				}
			}
		}
	}
}
