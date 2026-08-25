// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"bytes"
	"net"
	"os"
	"sync"
	"time"

	"github.com/omec-project/upfadapter/logger"
	"github.com/omec-project/upfadapter/types"
	"github.com/wmnsk/go-pfcp/message"
)

type UPFStatus int

const MaxUpfProbeRetryInterval time.Duration = 5 // Seconds

// PfcpPort mirrors udp.PFCP_PORT. It is repeated rather than imported because the udp
// package imports this one.
const PfcpPort = 8805

var UpfCfg Config

const (
	NotAssociated          UPFStatus = 0
	AssociatedSettingUp    UPFStatus = 1
	AssociatedSetUpSuccess UPFStatus = 2
)

// UPF structure
type UPNode struct {
	UpfName     string
	LastAssoRsp message.AssociationSetupResponse
	LastHBRsp   message.HeartbeatResponse
	ANIP        net.IP
	NodeID      types.NodeID
	State       UPFStatus
	UpfLock     sync.RWMutex
}

// All UPF nodes
type Config struct {
	UPFs        map[string]*UPNode
	UpfListLock sync.RWMutex
}

type UdpPodMsgType int

type adapterMessage struct {
	Body []byte `json:"body"`
}

type UdpPodPfcpMsg struct {
	Addr     *net.UDPAddr   `json:"addr"`
	SmfIp    string         `json:"smfIp"`
	Msg      adapterMessage `json:"pfcpMsg"`
	UpNodeID types.NodeID   `json:"upNodeID"`
}

type PfcpHttpRsp struct {
	Err error
	Rsp []byte
}

type PfcpTxnChan chan PfcpHttpRsp

var (
	UpfTxns      map[uint32]PfcpTxnChan
	UpfTxnsMutex = sync.RWMutex{}
)

var (
	UpfAdapterIp       net.IP
	UpfServerStartTime time.Time
)

func init() {
	podIpStr := os.Getenv("POD_IP")
	podIp := net.ParseIP(podIpStr)
	UpfAdapterIp = podIp.To4()

	UpfCfg = Config{
		UPFs: make(map[string]*UPNode),
	}

	UpfTxns = make(map[uint32]PfcpTxnChan)
}

func IsUpfAssociated(nodeId types.NodeID) bool {
	UpfCfg.UpfListLock.RLock()
	defer UpfCfg.UpfListLock.RUnlock()

	logger.CfgLog.Debugf("associated upfs: [%v]", UpfCfg.UPFs)

	if upf := UpfCfg.UPFs[string(nodeId.NodeIdValue)]; upf != nil {
		if upf.State == AssociatedSetUpSuccess {
			logger.CfgLog.Debugf("upf:[%v] associated", string(nodeId.NodeIdValue))
			return true
		}
		logger.CfgLog.Debugf("upf:[%v] not associated", string(nodeId.NodeIdValue))
		return false
	}

	logger.CfgLog.Debugf("upf:[%v] not configured yet", string(nodeId.NodeIdValue))
	return false
}

func GetUpfFromNodeId(nodeId *types.NodeID) *UPNode {
	UpfCfg.UpfListLock.RLock()
	defer UpfCfg.UpfListLock.RUnlock()

	logger.CfgLog.Debugf("getting upf from node id [%v] ", nodeId)
	logger.CfgLog.Debugf("content of upf config [%v] ", UpfCfg.UPFs)

	for _, upf := range UpfCfg.UPFs {
		if nodeId.NodeIdType == types.NodeIdTypeIpv4Address {
			if bytes.Equal(upf.ANIP.To4(), nodeId.NodeIdValue) {
				logger.CfgLog.Debugf("getting upf from node id, ip-addr [%v, %v] successful", nodeId, upf.ANIP.To4())
				return upf
			}
		} else if nodeId.NodeIdType == types.NodeIdTypeFqdn &&
			upf.NodeID.NodeIdType == types.NodeIdTypeFqdn {
			if bytes.Equal(nodeId.NodeIdValue, upf.NodeID.NodeIdValue) {
				logger.CfgLog.Debugf("getting upf from node id, fqdn [%v, %v] successful", nodeId, nodeId.NodeIdValue)
				return upf
			}
		}
	}
	logger.CfgLog.Errorf("getting upf from node id [%v] failure", nodeId)
	return nil
}

func InsertUpfNode(nodeId types.NodeID) {
	UpfCfg.UpfListLock.Lock()
	defer UpfCfg.UpfListLock.Unlock()

	// if UPF is already not added
	if _, ok := UpfCfg.UPFs[string(nodeId.NodeIdValue)]; !ok {
		upf := UPNode{
			UpfName: string(nodeId.NodeIdValue),
			State:   NotAssociated,
			NodeID:  nodeId,
			ANIP:    nodeId.ResolveNodeIdToIp(),
		}
		UpfCfg.UPFs[string(nodeId.NodeIdValue)] = &upf
		logger.CfgLog.Infof("inserting upf node [%v] ", string(nodeId.NodeIdValue))
	}
}

func ActivateUpfNode(nodeId *types.NodeID) *UPNode {
	logger.CfgLog.Infof("activating upf node [%v]", nodeId)
	if upf := GetUpfFromNodeId(nodeId); upf != nil {
		UpfCfg.UpfListLock.Lock()
		upf.State = AssociatedSetUpSuccess
		UpfCfg.UpfListLock.Unlock()
		return upf
	}
	logger.CfgLog.Errorf("upf node [%v] not found ", nodeId)
	return nil
}

var (
	smfAddrMutex sync.RWMutex
	smfAddr      string

	reportRelayMutex sync.Mutex
	reportRelays     = make(map[uint32]reportRelay)
	reportRelaySeq   uint32
)

const (
	// reportRelayLifetime bounds how long the origin of a relayed report is remembered. An
	// SMF that never answers must not cost an entry for the life of the process.
	reportRelayLifetime = 30 * time.Second

	// relaySequenceFloor is where the adapter's own sequence numbers start. Sequence
	// numbers are three octets, and the SMF counts up from zero, so counting down from the
	// top keeps the two apart for the life of any real deployment.
	relaySequenceFloor = 0x800000
	relaySequenceCeil  = 0xFFFFFF
)

type reportRelay struct {
	upfAddr  *net.UDPAddr
	upfSeq   uint32
	recorded time.Time
}

// SetSmfAddr records where the SMF talks to us from. Every SMF-initiated message
// carries it, and it is the only way the adapter can relay a message the user-plane
// function originates: those arrive with no request of ours to answer.
func SetSmfAddr(ip string) {
	if ip == "" {
		return
	}

	smfAddrMutex.Lock()
	defer smfAddrMutex.Unlock()

	if smfAddr != ip {
		logger.CfgLog.Infof("SMF address for relayed messages is now [%s]", ip)
	}

	smfAddr = ip
}

// SmfAddr returns the recorded SMF address, or nil if no SMF has spoken to us yet.
func SmfAddr() *net.UDPAddr {
	smfAddrMutex.RLock()
	defer smfAddrMutex.RUnlock()

	if smfAddr == "" {
		return nil
	}

	ip := net.ParseIP(smfAddr)
	if ip == nil {
		logger.CfgLog.Errorf("recorded SMF address [%s] is not an IP", smfAddr)
		return nil
	}

	return &net.UDPAddr{IP: ip, Port: PfcpPort}
}

// RelayReportSequence allocates the sequence number the adapter uses toward the SMF for a
// report a user-plane function raised, and remembers the origin so the answer can be
// returned to it carrying the number it is waiting for.
//
// The adapter has to number these itself. Outstanding requests are held in one table
// keyed by this socket's own address, so every request the adapter sends shares a single
// sequence space -- forwarding a UPF's number into it is refused as a duplicate whenever
// an SMF-originated request happens to be in flight under the same number, and the report
// is then rejected for no reason but coincidence. With several user planes, whose counters
// are independent of each other, two reports collide directly.
//
// Entries the SMF never answers are dropped once they are older than reportRelayLifetime.
func RelayReportSequence(upfAddr *net.UDPAddr, upfSeq uint32, now time.Time) uint32 {
	reportRelayMutex.Lock()
	defer reportRelayMutex.Unlock()

	for held, relay := range reportRelays {
		if now.Sub(relay.recorded) > reportRelayLifetime {
			delete(reportRelays, held)
			logger.CfgLog.Warnf("no response was relayed for session report seq[%d], forgetting it", relay.upfSeq)
		}
	}

	if reportRelaySeq < relaySequenceFloor || reportRelaySeq >= relaySequenceCeil {
		reportRelaySeq = relaySequenceFloor
	} else {
		reportRelaySeq++
	}

	reportRelays[reportRelaySeq] = reportRelay{upfAddr: upfAddr, upfSeq: upfSeq, recorded: now}

	return reportRelaySeq
}

// TakeReportRelay returns and forgets where a relayed report came from, together with the
// sequence number that user-plane function gave it. A zero address means the response
// matches no report the adapter relayed.
func TakeReportRelay(relaySeq uint32) (*net.UDPAddr, uint32) {
	reportRelayMutex.Lock()
	defer reportRelayMutex.Unlock()

	relay, ok := reportRelays[relaySeq]
	if !ok {
		return nil, 0
	}

	delete(reportRelays, relaySeq)

	return relay.upfAddr, relay.upfSeq
}

func InsertUpfPfcpTxn(seq uint32, pfcpTxnChan PfcpTxnChan) {
	logger.CfgLog.Debugf(" inserting transaction with sequence number [%v]", seq)
	UpfTxnsMutex.Lock()
	UpfTxns[seq] = pfcpTxnChan
	UpfTxnsMutex.Unlock()
}

func GetUpfPfcpTxn(seq uint32) PfcpTxnChan {
	UpfTxnsMutex.Lock()
	defer UpfTxnsMutex.Unlock()
	pfcpTxnChan := UpfTxns[seq]
	if pfcpTxnChan != nil {
		delete(UpfTxns, seq)
		logger.CfgLog.Debugf("fetch transaction with sequence number [%v] successful", seq)
		return pfcpTxnChan
	}
	logger.CfgLog.Errorf("fetch transaction with sequence number [%v] failure", seq)

	return nil
}

func (upf *UPNode) PreservePfcpAssociationRsp(pfcpRspBody message.AssociationSetupResponse) {
	// find the UPF
	logger.CfgLog.Debugf("storing pfcp association response for upf [%v] ", upf)
	upf.UpfLock.Lock()
	defer upf.UpfLock.Unlock()
	upf.LastAssoRsp = pfcpRspBody
}

func (upf *UPNode) PreservePfcpHeartBeatRsp(pfcpRspBody message.HeartbeatResponse) {
	// find the UPF
	logger.CfgLog.Debugf("storing pfcp heartbeat response for upf [%v] ", upf)
	upf.UpfLock.Lock()
	defer upf.UpfLock.Unlock()
	upf.LastHBRsp = pfcpRspBody
}
