// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
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
	smfAddr      net.IP

	upfAddrMutex sync.RWMutex
	// upfAddrs maps a user plane's address to the node identities currently reachable at
	// it, and upfNodeAddrs is the reverse, with the time the SMF last named each identity.
	// Two identities can share one address -- the SMF may name the same user plane by FQDN
	// in one message and by IP in another -- so an address stops being authorised only once
	// no identity is left at it.
	upfAddrs     = make(map[string]map[string]struct{})
	upfNodeAddrs = make(map[string]upfNodeRecord)

	reportRelayMutex sync.Mutex
	reportRelays     = make(map[uint32]reportRelay)
	reportRelaySeq   uint32
)

const (
	// reportRelayLifetime bounds how long the origin of a relayed report is remembered. An
	// SMF that never answers must not cost an entry for the life of the process.
	reportRelayLifetime = 30 * time.Second

	// upfAddrLifetime bounds how long an address stays authorised after the SMF stops naming
	// the user plane at it. Every PFCP message the SMF forwards through the adapter names its
	// user plane, heartbeats included, so a live node is renamed constantly and this only ever
	// reaches one that has gone quiet altogether. It is deliberately far longer than any
	// heartbeat period: expiring a node the SMF is still talking to would drop the session
	// reports this relay exists to carry.
	upfAddrLifetime = 30 * time.Minute

	// relaySequenceFloor is where the adapter's own sequence numbers start. Sequence
	// numbers are three octets and the SMF counts up from zero, so starting at the halfway
	// point keeps the two apart in an ordinary deployment -- but only that. The SMF's
	// counter reaches this half after enough messages, and nothing here can see its state,
	// so the separation is a convention and not isolation. What makes a collision harmless
	// is that the relay is retried under the next number when the transaction table refuses
	// one already in flight; see udp.ErrDuplicateSequence.
	relaySequenceFloor = 0x800000
	relaySequenceCeil  = 0xFFFFFF
)

// upfNodeRecord is where one user plane was last reached and when the SMF last named it. The
// address is empty for an identity whose name does not currently resolve: the SMF is still
// addressing it, so it has not gone away, but there is no address to authorise for it.
type upfNodeRecord struct {
	seen time.Time
	addr string
}

// ErrRelaySequenceExhausted reports that every sequence number the adapter allocates from is
// outstanding. It is separate from a retransmission because the answer is different: the report
// cannot be relayed at all, and the user plane has to be told so rather than left waiting.
var ErrRelaySequenceExhausted = errors.New("no free relay sequence number")

type reportRelay struct {
	// Ordered so the fields carrying pointers lead: govet's fieldalignment counts the leading
	// pointer bytes, and time.Time holds one.
	upfAddr  *net.UDPAddr
	recorded time.Time
	upfSeq   uint32
	// answered marks the entry as claimed by whichever path is returning an answer to the
	// user plane, while leaving it in the map. It is what keeps the two apart: only the
	// first taker acts, and the entry goes on absorbing retransmissions until that path
	// has finished.
	answered bool
}

// SetSmfAddr records where the SMF talks to us from. Every SMF-initiated message
// carries it, and it is the only way the adapter can relay a message the user-plane
// function originates: those arrive with no request of ours to answer.
//
// A claim that is not an IP is refused, and what is already recorded kept. The field
// comes from an unauthenticated request body, and storing it unparsed meant a single
// malformed claim erased a working address -- every report was then rejected until the
// next SMF message happened to carry a good one.
func SetSmfAddr(ip string) {
	if ip == "" {
		return
	}

	parsed := net.ParseIP(ip)
	if parsed == nil {
		logger.CfgLog.Errorf("ignoring claimed SMF address [%s]: not an IP", ip)
		return
	}

	// 0.0.0.0 and :: parse, and neither is a destination: relayed reports sent there go
	// nowhere and time out. Refusing keeps a working address rather than letting one
	// claim carrying an unspecified value erase it -- the same reason a claim that is not
	// an IP is refused, and the same test RecordUpfAddr applies to a user plane.
	if parsed.IsUnspecified() {
		logger.CfgLog.Errorf("ignoring claimed SMF address [%s]: the unspecified address is not a destination", ip)
		return
	}

	smfAddrMutex.Lock()
	defer smfAddrMutex.Unlock()

	if !smfAddr.Equal(parsed) {
		logger.CfgLog.Infof("SMF address for relayed messages is now [%s]", parsed)
	}

	smfAddr = parsed
}

// SmfAddr returns the recorded SMF address, or nil if no SMF has spoken to us yet.
func SmfAddr() *net.UDPAddr {
	smfAddrMutex.RLock()
	defer smfAddrMutex.RUnlock()

	if smfAddr == nil {
		return nil
	}

	// A copy: net.IP is a slice, and this one is handed to every caller while the
	// package keeps writing the field.
	return &net.UDPAddr{IP: slices.Clone(smfAddr), Port: PfcpPort}
}

// RecordUpfAddr remembers a user-plane function the SMF has addressed through us, so a
// message that user plane originates can be told apart from one anybody else sent.
//
// The set is learned from ordinary SMF traffic rather than taken from UpfCfg.UPFs. That
// table is filled only on the association-setup path, and an adapter restart empties it
// while the sessions it would guard stay established: the SMF is given the user plane's
// own recovery timestamp on every heartbeat response, not ours, so it has no reason to
// associate again and the table would stay empty for the life of the association. Every
// message the SMF forwards names its user plane, so this set comes back within one
// heartbeat instead -- the same way the SMF's own address does.
// It returns the addresses it stopped authorising, so the caller can release the per-peer state
// they were holding. Nothing will be relayed or answered for them again without the SMF naming
// them afresh, which records them here first.
func RecordUpfAddr(nodeId *types.NodeID) []string {
	ip := nodeId.ResolveNodeIdToIp()
	node := nodeKey(nodeId)
	now := time.Now()

	upfAddrMutex.Lock()
	defer upfAddrMutex.Unlock()

	// This message is the SMF naming the node, so record that before the sweep rather than
	// after it. The other order expires a node whose name has not resolved for longer than the
	// lifetime on the very message that shows it is still in use.
	record := upfNodeAddrs[node]
	record.seen = now
	upfNodeAddrs[node] = record

	expired := expireUpfAddrsLocked(now)

	if ip == nil || ip.IsUnspecified() {
		// An FQDN that does not resolve yields IPv4zero, and there is no address to
		// authorise. The identity stays named, because a name that fails to resolve for a
		// moment is not a user plane that has gone: revoking here would stop relaying for a
		// node that is merely waiting on DNS. What the record buys is the other case -- a
		// node the SMF stops naming altogether ages out, and its address with it.
		return expired
	}

	key := ip.String()

	// A node that moved stops authorising the address it left. Without this the set only
	// ever grows: a user plane reached through a name, or one that came back on a new
	// address, leaves its old address authorised for the life of the process, and reports
	// from whatever occupies that address next are relayed as if the SMF had named it.
	released := expired

	if record.addr != "" && record.addr != key {
		if gone := forgetUpfAddrLocked(node, record.addr); gone {
			released = append(released, record.addr)
		}
	}

	if _, known := upfAddrs[key]; !known {
		// The resolved address only: NodeIdValue is four raw octets for an IPv4 node ID,
		// which prints as rubbish.
		logger.CfgLog.Infof("the SMF addresses a user plane at [%s]; reports from it will be relayed", key)
		upfAddrs[key] = make(map[string]struct{})
	}

	upfAddrs[key][node] = struct{}{}
	upfNodeAddrs[node] = upfNodeRecord{addr: key, seen: now}

	return released
}

// expireUpfAddrsLocked drops the user planes the SMF has stopped naming, and the addresses left
// with nothing at them. Callers hold upfAddrMutex.
//
// It runs from RecordUpfAddr rather than on a timer because every message the SMF forwards passes
// through there, so the sweep happens exactly as often as there is anything to sweep.
func expireUpfAddrsLocked(now time.Time) []string {
	var released []string

	for node, record := range upfNodeAddrs {
		if now.Sub(record.seen) <= upfAddrLifetime {
			continue
		}

		delete(upfNodeAddrs, node)

		if record.addr != "" {
			logger.CfgLog.Infof("the SMF has not named the user plane at [%s] for %s; it is no longer relayed for",
				record.addr, upfAddrLifetime)

			if gone := forgetUpfAddrLocked(node, record.addr); gone {
				released = append(released, record.addr)
			}
		}
	}

	return released
}

// nodeKey identifies one user plane across address changes. The type is part of it because
// the same octets mean different things under different node id types.
func nodeKey(nodeId *types.NodeID) string {
	return fmt.Sprintf("%d/%s", nodeId.NodeIdType, nodeId.NodeIdValue)
}

// forgetUpfAddrLocked drops one identity from an address, and the address with it once no
// identity is left there. It reports whether the address itself went away, so a caller can
// release what it was holding for that peer. Callers hold upfAddrMutex.
func forgetUpfAddrLocked(node, addr string) bool {
	nodes, known := upfAddrs[addr]
	if !known {
		return false
	}

	delete(nodes, node)

	if len(nodes) > 0 {
		return false
	}

	delete(upfAddrs, addr)
	logger.CfgLog.Infof("no user plane the SMF has named is at [%s] any more; reports from it will not be relayed", addr)

	return true
}

// IsKnownUpfAddr reports whether a peer is one of the user-plane functions the SMF has
// addressed through us, and has named recently enough. Nothing else may have a message
// relayed on its behalf, or be answered by us.
//
// The age is tested here rather than left to the sweep, because the sweep runs on SMF traffic
// and this question is asked on user-plane traffic. An SMF that has gone quiet is exactly the
// case the lifetime exists for, and it is also the case in which nothing would sweep: reports
// would go on being relayed for an address whose user plane the SMF stopped naming half an hour
// ago. Removing the entry still waits for the next SMF message; this only declines to answer on
// its behalf in the meantime.
func IsKnownUpfAddr(ip net.IP) bool {
	if ip == nil {
		return false
	}

	upfAddrMutex.RLock()
	defer upfAddrMutex.RUnlock()

	now := time.Now()

	for node := range upfAddrs[ip.String()] {
		if now.Sub(upfNodeAddrs[node].seen) <= upfAddrLifetime {
			return true
		}
	}

	return false
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
//
// fresh is false when this user plane already has a report outstanding under this
// sequence number: that is a retransmission, and the relay already in flight answers it.
// Nothing else absorbs those. Answering a report creates a response transaction that
// holds it for the resend window, so a retransmission arriving *after* the answer is met
// with the answer again -- but until then the report has no transaction here at all, and
// every copy would be renumbered and forwarded separately, raising a downlink data
// notification each time for traffic the SMF is already being told about.
func RelayReportSequence(upfAddr *net.UDPAddr, upfSeq uint32, now time.Time) (relaySeq uint32, fresh bool, err error) {
	reportRelayMutex.Lock()
	defer reportRelayMutex.Unlock()

	var (
		outstanding uint32
		relaying    bool
	)

	// The sweep walks the whole map already, so recognising a retransmission rides along
	// with it rather than costing an index of its own.
	for held, relay := range reportRelays {
		if now.Sub(relay.recorded) > reportRelayLifetime {
			delete(reportRelays, held)
			// Both numbers, and the peer: the map is keyed by the adapter's own sequence while the
			// user plane is waiting on its own, and several user planes can be waiting on the same
			// one. Reporting only the user plane's number names an entry that cannot be looked up.
			logger.CfgLog.Warnf("no response was relayed for session report from %v: adapter seq[%d], user plane seq[%d]; forgetting it",
				relay.upfAddr, held, relay.upfSeq)

			continue
		}

		if relay.upfSeq == upfSeq && relay.upfAddr.IP.Equal(upfAddr.IP) && relay.upfAddr.Port == upfAddr.Port {
			outstanding, relaying = held, true
		}
	}

	if relaying {
		return outstanding, false, nil
	}

	// Advance to a number nothing is waiting on. Assigning the next one unconditionally is fine
	// until the counter comes round: an entry still outstanding would then be replaced, and the
	// SMF's answer to the first report would be returned to the user plane that raised the
	// second, while the first exchange was never answered at all.
	candidate, free := nextFreeRelaySequence(reportRelays, reportRelaySeq, relaySequenceFloor, relaySequenceCeil)
	if !free {
		return 0, false, ErrRelaySequenceExhausted
	}

	reportRelaySeq = candidate
	reportRelays[candidate] = reportRelay{upfAddr: upfAddr, upfSeq: upfSeq, recorded: now}

	return candidate, true, nil
}

// nextFreeRelaySequence returns the first number in [floor, ceil] that nothing is waiting on,
// searching from the one after `from` and wrapping. It reports false when every number in the
// range is outstanding.
//
// The trip count is one per assignable number, and the range is inclusive at both ends: the
// counter resets only once it is past the ceiling, so the ceiling itself is handed out. One fewer
// trip skips exactly one number -- the one the search started from, which the wrap reaches last --
// and that is the number still free when every other is taken.
//
// It is a function of its arguments so the arithmetic can be tested over a range small enough to
// fill; the live range holds 8 388 608 numbers.
func nextFreeRelaySequence(outstanding map[uint32]reportRelay, from, floor, ceil uint32) (uint32, bool) {
	candidate := from

	for range ceil - floor + 1 {
		if candidate < floor || candidate >= ceil {
			candidate = floor
		} else {
			candidate++
		}

		if _, taken := outstanding[candidate]; !taken {
			return candidate, true
		}
	}

	return 0, false
}

// TakeReportRelay claims a relayed report for answering and returns where it came from,
// together with the sequence number that user-plane function gave it. A nil address means
// the report matches nothing the adapter is relaying, or that another path has already
// claimed it -- the SMF's answer and the give-up path both end the same exchange, and only
// one of them may answer it.
//
// The entry is kept, not deleted, until ForgetReportRelay. Deleting here left a window in
// which a retransmitted report matched neither an entry here nor a response transaction --
// which is only created once the answer is sent -- and was therefore relayed a second time,
// raising a second downlink data notification and a second page. Messages are dispatched on
// their own goroutines, so that window is reachable.
func TakeReportRelay(relaySeq uint32) (*net.UDPAddr, uint32) {
	reportRelayMutex.Lock()
	defer reportRelayMutex.Unlock()

	relay, ok := reportRelays[relaySeq]
	if !ok || relay.answered {
		return nil, 0
	}

	relay.answered = true
	reportRelays[relaySeq] = relay

	return relay.upfAddr, relay.upfSeq
}

// ForgetReportRelay drops the entry once the answer to that report has been sent, or once
// the relay it was holding turned out never to have gone out at all.
func ForgetReportRelay(relaySeq uint32) {
	reportRelayMutex.Lock()
	defer reportRelayMutex.Unlock()

	delete(reportRelays, relaySeq)
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
