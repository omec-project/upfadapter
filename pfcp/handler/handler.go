// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
package handler

import (
	"fmt"
	"net"
	"time"

	"github.com/omec-project/upfadapter/config"
	"github.com/omec-project/upfadapter/logger"
	"github.com/omec-project/upfadapter/pfcp/udp"
	"github.com/omec-project/upfadapter/types"
	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"
)

func HandlePfcpSendError(msg message.Message, pfcpErr error) {
	msgType := msg.MessageType()
	logger.PfcpLog.Errorf("send of PFCP msg [%v] failed with error [%v]",
		msgType, pfcpErr.Error())
	switch msgType {
	case message.MsgTypeAssociationSetupRequest:
		handleSendPfcpAssoSetReqError(msg, pfcpErr)
	case message.MsgTypeHeartbeatRequest:
		handleSendPfcpHeartbeatReqError(msg, pfcpErr)
	case message.MsgTypeSessionEstablishmentRequest:
		handleSendPfcpSessEstReqError(msg, pfcpErr)
	case message.MsgTypeSessionModificationRequest:
		handleSendPfcpSessModReqError(msg, pfcpErr)
	case message.MsgTypeSessionDeletionRequest:
		handleSendPfcpSessRelReqError(msg, pfcpErr)
	default:
		logger.PfcpLog.Errorf("unable to send PFCP packet type [%v] and content [%v]",
			msgType, msg)
	}
}

func handleSendPfcpAssoSetReqError(msg message.Message, pfcpErr error) {
	logger.PfcpLog.Debugf("send association setup request error [%v]", pfcpErr.Error())
	// send Error
	sendErrRsp(msg, pfcpErr)
}

func handleSendPfcpHeartbeatReqError(msg message.Message, pfcpErr error) {
	logger.PfcpLog.Debugf("send heartbeat request error [%v]", pfcpErr.Error())
	// send Error
	sendErrRsp(msg, pfcpErr)
}

func handleSendPfcpSessEstReqError(msg message.Message, pfcpErr error) {
	logger.PfcpLog.Debugf("send session establishment request error [%v]", pfcpErr.Error())
	// send Error
	sendErrRsp(msg, pfcpErr)
}

func handleSendPfcpSessModReqError(msg message.Message, pfcpErr error) {
	logger.PfcpLog.Debugf("send session modification request error [%v]", pfcpErr.Error())
	// send Error
	sendErrRsp(msg, pfcpErr)
}

func handleSendPfcpSessRelReqError(msg message.Message, pfcpErr error) {
	logger.PfcpLog.Debugf("send session release request error [%v]", pfcpErr.Error())
	// send Error
	sendErrRsp(msg, pfcpErr)
}

func sendErrRsp(msg message.Message, err error) {
	// Get the PFCP Txn
	pfcpTxnChan := config.GetUpfPfcpTxn(msg.Sequence())

	// Send Rsp back to http txn
	pfcpTxnChan <- config.PfcpHttpRsp{Rsp: nil, Err: err}
}

func encodeAndSendRsp(msg message.Message) error {
	buf := make([]byte, msg.MarshalLen())
	err := msg.MarshalTo(buf)
	if err != nil {
		return err
	}

	// Get the PFCP Txn
	pfcpTxnChan := config.GetUpfPfcpTxn(msg.Sequence())

	// Send Rsp back to http txn
	pfcpTxnChan <- config.PfcpHttpRsp{Rsp: buf, Err: nil}

	return nil
}

func HandlePfcpAssociationSetupResponse(msg message.Message) {
	rsp, ok := msg.(*message.AssociationSetupResponse)
	if !ok {
		logger.PfcpLog.Errorln("invalid PFCP Association Setup Response")
		return
	}

	recoveryTimeStamp, err := rsp.RecoveryTimeStamp.RecoveryTimeStamp()
	if err != nil {
		logger.PfcpLog.Errorf("failed to parse recovery timestamp: %v", err)
		return
	}

	logger.PfcpLog.Debugf("handle pfcp association setup response, recovery timestamp [%v]", recoveryTimeStamp)

	cause, err := rsp.Cause.Cause()
	if err != nil {
		logger.PfcpLog.Errorf("failed to parse cause: %v", err)
		return
	}

	if cause == ie.CauseRequestAccepted {
		// UPF's node ID
		nodeIDstr, err := rsp.NodeID.NodeID()
		if err != nil {
			logger.PfcpLog.Errorf("failed to parse node id: %v", err)
			return
		}
		// Add UPF as active
		logger.PfcpLog.Debugf("node id from pfcp association response [%v]", nodeIDstr)
		nodeId := types.NewNodeID(nodeIDstr)
		upf := config.ActivateUpfNode(nodeId)

		// Preserve success Asso Rsp
		upf.PreservePfcpAssociationRsp(*rsp)
	}

	// Encode pfcp rsp to byte and send to http txn
	if err := encodeAndSendRsp(msg); err != nil {
		logger.PfcpLog.Errorf("handle pfcp association response error [%v]", err)
	}
}

func HandlePfcpHeartbeatResponse(msg message.Message) {
	heartbeatResp, ok := msg.(*message.HeartbeatResponse)
	if !ok {
		logger.PfcpLog.Errorln("invalid PFCP Heartbeat Response")
		return
	}
	recoveryTimestamp, err := heartbeatResp.RecoveryTimeStamp.RecoveryTimeStamp()
	if err != nil {
		logger.PfcpLog.Errorf("failed to parse recovery timestamp: %v", err)
		return
	}
	logger.PfcpLog.Debugf("pfcp heartbeat response recovery timestamp [%v]", recoveryTimestamp)
	// Encode pfcp rsp to byte and send to http txn
	if err := encodeAndSendRsp(msg); err != nil {
		logger.PfcpLog.Errorf("handle pfcp heartbeat response error [%v]", err)
	}
}

func HandlePfcpSessionEstablishmentResponse(msg message.Message) {
	_, ok := msg.(*message.SessionEstablishmentResponse)
	if !ok {
		logger.PfcpLog.Errorln("invalid PFCP Session Establishment Response")
		return
	}
	// Encode pfcp rsp to byte and send to http txn
	if err := encodeAndSendRsp(msg); err != nil {
		logger.PfcpLog.Errorf("handle pfcp session establishment response error [%v]", err)
	}
}

func HandlePfcpSessionModificationResponse(msg message.Message) {
	_, ok := msg.(*message.SessionModificationResponse)
	if !ok {
		logger.PfcpLog.Errorln("invalid PFCP Session Modification Response")
		return
	}
	// Encode pfcp rsp to byte and send to http txn
	if err := encodeAndSendRsp(msg); err != nil {
		logger.PfcpLog.Errorf("handle pfcp session modify response error [%v]", err)
	}
}

// HandlePfcpSessionReportRequest relays a report the user-plane function originated to
// the SMF, and remembers where it came from so the answer can be returned.
//
// Nothing used to handle this message: it fell to the dispatcher's default branch and
// was logged as unknown. A downlink data notification is the only way the SMF learns
// that an idle UE has traffic waiting, so dropping it removed mobile-terminated
// reachability from every deployment that puts this adapter on N4 -- silently, because
// the user-plane function's request simply went unanswered.
func HandlePfcpSessionReportRequest(msg message.Message, upfAddr *net.UDPAddr) {
	report, ok := msg.(*message.SessionReportRequest)
	if !ok {
		logger.PfcpLog.Errorln("invalid PFCP Session Report Request")
		return
	}

	if upfAddr == nil {
		logger.PfcpLog.Errorln("session report request with no peer address, cannot relay or answer")
		return
	}

	upfSeq := report.Sequence()

	smfAddr := config.SmfAddr()
	if smfAddr == nil {
		// Answer rather than stay silent: an unanswered request leaves the user-plane
		// function retransmitting into nothing, and a rejection at least tells it the
		// traffic will not be delivered.
		logger.PfcpLog.Errorln("no SMF address known yet, rejecting session report request")
		rejectSessionReport(report.SEID(), upfSeq, upfAddr)

		return
	}

	// Renumber into the adapter's own sequence space before relaying; the UPF's number
	// goes back on the response. See config.RelayReportSequence.
	relaySeq := config.RelayReportSequence(upfAddr, upfSeq, time.Now())
	report.SetSequenceNumber(relaySeq)

	// If the SMF never answers, tell the user-plane function so. Its own retransmissions
	// would otherwise be the only thing that ends the wait, and TS 29.244 expects every
	// request to be answered. TakeReportRelay is take-once, so this and the response path
	// cannot both answer the same report.
	seid := report.SEID()
	eventData := udp.PfcpEventData{LSEID: 0, ErrHandler: func(sent message.Message, sendErr error) {
		logger.PfcpLog.Errorf("relayed session report seq[%d] was not answered by the SMF: %v",
			sent.Sequence(), sendErr)

		if addr, seq := config.TakeReportRelay(sent.Sequence()); addr != nil {
			rejectSessionReport(seid, seq, addr)
		}
	}}

	if err := udp.SendPfcp(report, smfAddr, eventData); err != nil {
		logger.PfcpLog.Errorf("failed to relay session report request to SMF [%v]: %v", smfAddr, err)
		config.TakeReportRelay(relaySeq)
		rejectSessionReport(seid, upfSeq, upfAddr)

		return
	}

	logger.PfcpLog.Infof("relayed session report seq[%d] from UPF [%v] to SMF [%v] as seq[%d]",
		upfSeq, upfAddr, smfAddr, relaySeq)
}

// HandlePfcpSessionReportResponse returns the SMF's answer to the user-plane function
// that raised the report. Unlike the other responses this one is not the tail of an
// SMF-initiated exchange, so there is no HTTP transaction waiting for it.
func HandlePfcpSessionReportResponse(msg message.Message) {
	response, ok := msg.(*message.SessionReportResponse)
	if !ok {
		logger.PfcpLog.Errorln("invalid PFCP Session Report Response")
		return
	}

	relaySeq := response.Sequence()

	upfAddr, upfSeq := config.TakeReportRelay(relaySeq)
	if upfAddr == nil {
		logger.PfcpLog.Warnf("session report response seq[%d] matches no relayed report, dropping",
			relaySeq)
		return
	}

	// The user-plane function is waiting for its own sequence number, not ours.
	response.SetSequenceNumber(upfSeq)

	if err := udp.SendPfcp(response, upfAddr, reportResponseEventData()); err != nil {
		logger.PfcpLog.Errorf("failed to return session report response to UPF [%v]: %v", upfAddr, err)
		return
	}

	logger.PfcpLog.Infof("returned session report response seq[%d] to UPF [%v]", upfSeq, upfAddr)
}

// reportResponseEventData reports the end of a response's resend window for what it is.
// A response transaction holds the message so a retransmitted report is answered again,
// and closes with a timeout once the peer stops asking -- the ordinary outcome of a
// delivered response. HandlePfcpSendError would announce that as a message it was unable
// to send, and a false negative on this path is what made the original defect so hard to
// find.
func reportResponseEventData() udp.PfcpEventData {
	return udp.PfcpEventData{LSEID: 0, ErrHandler: func(msg message.Message, err error) {
		logger.PfcpLog.Debugf("resend window closed for session report response seq[%d]: %v",
			msg.Sequence(), err)
	}}
}

// rejectSessionReport answers a report the adapter cannot relay, so the user-plane
// function stops waiting and can release what it was holding. It is answered under the
// sequence number that user plane used, which is not the one the report may since have
// been renumbered to.
func rejectSessionReport(seid uint64, upfSeq uint32, upfAddr *net.UDPAddr) {
	rsp := message.NewSessionReportResponse(0, 0, seid, upfSeq, 0,
		ie.NewCause(ie.CauseRequestRejected))

	if err := udp.SendPfcp(rsp, upfAddr, reportResponseEventData()); err != nil {
		logger.PfcpLog.Errorf("failed to reject session report request to UPF [%v]: %v", upfAddr, err)
	}
}

func HandlePfcpSessionDeletionResponse(msg message.Message) {
	_, ok := msg.(*message.SessionDeletionResponse)
	if !ok {
		logger.PfcpLog.Errorln("invalid PFCP Session Deletion Response")
		return
	}
	// Encode pfcp rsp to byte and send to http txn
	if err := encodeAndSendRsp(msg); err != nil {
		logger.PfcpLog.Errorf("handle pfcp session delete response error [%v]", err)
	}
}

func BuildPfcpAssociationResponse(nodeId *types.NodeID, seqNo uint32) (*message.AssociationSetupResponse, error) {
	logger.AppLog.Debugf("building pfcp association response for upf [%v], seqNo [%v]", nodeId, seqNo)
	upf := config.GetUpfFromNodeId(nodeId)
	if upf == nil {
		logger.AppLog.Errorf("upf [%v] not found", string(nodeId.NodeIdValue))
		return nil, fmt.Errorf("upf not found: %v", string(nodeId.NodeIdValue))
	}

	upf.UpfLock.RLock()
	lastAssociationRsp := upf.LastAssoRsp
	header := *lastAssociationRsp.Header
	upf.UpfLock.RUnlock()

	logger.AppLog.Debugf("stored association response recovery timestamp: %v", lastAssociationRsp.RecoveryTimeStamp)
	// clone the header so we don't mutate the stored response shared with other callers
	lastAssociationRsp.Header = &header
	lastAssociationRsp.SequenceNumber = seqNo
	return &lastAssociationRsp, nil
}
