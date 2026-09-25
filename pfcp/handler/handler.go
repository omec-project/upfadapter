// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
package handler

import (
	"errors"
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

// relaySendAttempts bounds how many sequence numbers a single report may try before it is
// rejected. Each retry costs one map insertion, and a peer numbering requests fast enough to
// take several of the adapter's numbers in a row is not a peer more attempts would help.
const relaySendAttempts = 4

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
	if pfcpTxnChan == nil {
		// Nothing is waiting on this sequence number -- the request never went out, or this is a
		// second response to one already answered. Sending into the nil channel that a missing
		// entry yields blocks this goroutine for the life of the process.
		logger.PfcpLog.Warnf("no request is waiting for seq[%d]; dropping the failure meant for it: %v",
			msg.Sequence(), err)

		return
	}

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
	if pfcpTxnChan == nil {
		return fmt.Errorf("no request is waiting for seq[%d]", msg.Sequence())
	}

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

	// Relay for the user planes the SMF has addressed through us, and answer those only.
	// N4 carries no transport authentication, so without this the adapter relays a report
	// for whatever can reach the port and -- because a report it cannot relay is rejected
	// rather than dropped -- answers it too, holding a resend transaction, a timer and a
	// never-reclaimed ConsumerTable entry per source address on its behalf. Dropping costs
	// a legitimate peer nothing: a source the SMF has never named has no session here and
	// so is holding no traffic to be told about. This runs before the rejection below so
	// that the only peers ever answered are ones the SMF named.
	// Read before the test, not after: what the claim below needs to know is whether a release
	// happened between the two, and a release that happened before this read is caught by the test
	// itself.
	authorisedUnder := config.RelayGeneration(upfAddr.IP.String())

	if !config.IsKnownUpfAddr(upfAddr.IP) {
		logger.PfcpLog.Warnf("session report request from [%v], which is not a user plane the SMF has addressed through us; dropping it", upfAddr)
		return
	}

	upfSeq := report.Sequence()

	smfAddr := config.SmfAddr()
	if smfAddr == nil {
		// Answer rather than stay silent: an unanswered request leaves the user-plane
		// function retransmitting into nothing, and a rejection at least tells it the
		// traffic will not be delivered.
		logger.PfcpLog.Errorln("no SMF address known yet, rejecting session report request")
		if err := rejectSessionReport(report.SEID(), upfSeq, upfAddr); err != nil {
			logger.PfcpLog.Errorf("session report seq[%d] from UPF [%v] could not be refused: %v",
				upfSeq, upfAddr, err)
		}

		return
	}

	// If the SMF never answers, tell the user-plane function so. Its own retransmissions
	// would otherwise be the only thing that ends the wait, and TS 29.244 expects every
	// request to be answered. TakeReportRelay is take-once, so this and the response path
	// cannot both answer the same report.
	seid := report.SEID()
	eventData := udp.PfcpEventData{LSEID: 0, ErrHandler: func(sent message.Message, sendErr error) {
		logger.PfcpLog.Errorf("relayed session report seq[%d] was not answered by the SMF: %v",
			sent.Sequence(), sendErr)

		// Forgotten only by the path that claimed it. Claiming can fail because the SMF's
		// answer arrived first and is still being sent, and dropping the entry from here
		// would reopen the window that entry is holding shut.
		if addr, seq := config.TakeReportRelay(sent.Sequence()); addr != nil {
			refuseAndRelease(seid, seq, addr, sent.Sequence())
		}
	}}

	// Renumber into the adapter's own sequence space before relaying; the UPF's number
	// goes back on the response. See config.RelayReportSequence.
	//
	// Claimed once, outside the loop. The loop is the send, which can be refused because the SMF
	// is using the number, and the claim has to outlive that refusal: it is what a retransmission
	// arriving meanwhile is recognised by.
	relaySeq, fresh, err := config.RelayReportSequence(upfAddr, upfSeq, time.Now(), authorisedUnder)
	if errors.Is(err, config.ErrPeerReleased) {
		// The address stopped being one the SMF names between the check above and this claim. The
		// report is dropped rather than refused, for the same reason an unknown source is: a peer
		// the adapter no longer relays for is holding no traffic this can be about.
		logger.PfcpLog.Warnf("session report seq[%d] from UPF [%v]: the SMF stopped naming it while the report was being claimed; dropping it",
			upfSeq, upfAddr)

		return
	}

	if errors.Is(err, config.ErrRelaySequenceExhausted) {
		// Nothing to relay under. Answering is still better than silence: the user plane
		// stops waiting and learns the traffic will not be delivered.
		logger.PfcpLog.Errorf("session report seq[%d] from UPF [%v] cannot be relayed: %v",
			upfSeq, upfAddr, err)
		// Nothing is claimed on this path, so there is nothing a failed refusal could strand.
		if refuseErr := rejectSessionReport(seid, upfSeq, upfAddr); refuseErr != nil {
			logger.PfcpLog.Errorf("session report seq[%d] from UPF [%v] could not be refused: %v",
				upfSeq, upfAddr, refuseErr)
		}

		return
	}

	if !fresh {
		// A retransmission of a report still being relayed. Relaying it again would raise a
		// second downlink data notification for traffic the SMF is already being told about;
		// the relay in flight answers this copy too, and the SMF gets the adapter's own
		// retransmissions meanwhile.
		logger.PfcpLog.Infof("session report seq[%d] from UPF [%v] is already being relayed as seq[%d]; not relaying it again",
			upfSeq, upfAddr, relaySeq)

		return
	}

	for range relaySendAttempts {
		report.SetSequenceNumber(relaySeq)

		err = udp.SendPfcp(report, smfAddr, eventData)
		if err == nil {
			logger.PfcpLog.Infof("relayed session report seq[%d] from UPF [%v] to SMF [%v] as seq[%d]",
				upfSeq, upfAddr, smfAddr, relaySeq)

			return
		}

		if errors.Is(err, udp.ErrDuplicateSequence) {
			// A request the SMF numbered is in flight under this number: its counter has
			// reached the half the adapter allocates from, which the adapter cannot see and
			// must not depend on. The next number is a free one, so the report is relayed
			// rather than rejected for a coincidence.
			//
			// Renumbered rather than forgotten and claimed again: the report stays claimed
			// throughout, so a retransmission arriving during the retry is still recognised as
			// one. Dropping it first left it unclaimed, and that copy was relayed on its own
			// account -- one report from one user plane becoming two notifications to the SMF.
			logger.PfcpLog.Warnf("relay sequence [%d] for session report seq[%d] from UPF [%v] is already in flight; trying the next one",
				relaySeq, upfSeq, upfAddr)

			renumbered, renumberErr := config.RenumberReportRelay(relaySeq)

			if errors.Is(renumberErr, config.ErrRelayNotHeld) {
				// The claim is gone: the address stopped being one the SMF names while this report
				// was being relayed, and the release took its claim with it. Answering now would
				// create a response transaction for a peer the source gate has already revoked --
				// and, if the address has been taken by someone else, hold it for them. Dropped
				// instead, as a report from an unknown source is.
				logger.PfcpLog.Warnf("session report seq[%d] from UPF [%v]: its claim was released while it was being relayed; dropping it",
					upfSeq, upfAddr)

				return
			}

			if renumberErr != nil {
				logger.PfcpLog.Errorf("session report seq[%d] from UPF [%v] could not be relayed under another number: %v",
					upfSeq, upfAddr, renumberErr)
				refuseAndRelease(seid, upfSeq, upfAddr, relaySeq)

				return
			}

			relaySeq = renumbered

			continue
		}

		logger.PfcpLog.Errorf("failed to relay session report request to SMF [%v]: %v", smfAddr, err)
		refuseAndRelease(seid, upfSeq, upfAddr, relaySeq)

		return
	}

	// Every number tried was in flight. The claim goes with the attempt -- nothing is being relayed
	// for this report, so a later copy of it must be free to start its own relay -- but only once
	// the refusal is registered to absorb the copies arriving meanwhile.
	logger.PfcpLog.Errorf("no free sequence number for session report seq[%d] from UPF [%v] in %d attempts; rejecting it",
		upfSeq, upfAddr, relaySendAttempts)
	refuseAndRelease(seid, upfSeq, upfAddr, relaySeq)
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

	// Only once the answer is on its way: sending it creates the response transaction that
	// absorbs further copies of the report, and until that exists this entry is the only
	// thing standing between a retransmission and a second relay.
	//
	// Registering that transaction is not writing it, and the claim goes either way. An answer
	// that never reached the wire has answered nothing, so the user plane's next copy has to be
	// relayed afresh; a copy absorbed by the claim of an answer that was never delivered would be
	// answered by nothing at all.
	defer config.ForgetReportRelay(relaySeq)

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
// refuseAndRelease tells the user plane its report will not be relayed, and only then ends the
// claim on it.
//
// The order is the point. A report is absorbed by its claim while it is being relayed, and by the
// response transaction once it has been answered -- sending the refusal registers that transaction
// before this returns. Ending the claim first left an instant in which neither held it, and a
// retransmission arriving there was taken for a new report and relayed on its own account: the
// second downlink data notification this whole path exists to prevent.
//
// The claim ends even when the refusal could not be sent. Nothing is in flight for the report
// then and nothing has answered it, so relaying the user plane's next copy afresh is the only
// route left to an answer. Holding the claim instead absorbs every copy until it ages out, which
// buys the certainty that the report is never answered at all -- and on the common way here, a
// relay that never reached the SMF, there is no notification for a second one to duplicate.
func refuseAndRelease(seid uint64, upfSeq uint32, upfAddr *net.UDPAddr, relaySeq uint32) {
	if err := rejectSessionReport(seid, upfSeq, upfAddr); err != nil {
		logger.PfcpLog.Errorf("session report seq[%d] from UPF [%v] could not be refused either: %v",
			upfSeq, upfAddr, err)
	}

	config.ForgetReportRelay(relaySeq)
}

func rejectSessionReport(seid uint64, upfSeq uint32, upfAddr *net.UDPAddr) error {
	rsp := message.NewSessionReportResponse(0, 0, seid, upfSeq, 0,
		ie.NewCause(ie.CauseRequestRejected))

	// Reported rather than logged here, because sending is what registers the transaction that
	// absorbs retransmissions of this report: a caller that releases the claim afterwards is
	// relying on this having happened, and only the caller knows what it was holding.
	return udp.SendPfcp(rsp, upfAddr, reportResponseEventData())
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
