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
	// relaysByHost indexes the same entries by the address that raised them, so recognising a
	// retransmission costs the reports outstanding from one user plane rather than from all of
	// them, and so a peer that stops being authorised can have its origins dropped without a
	// walk. It holds keys into reportRelays and is maintained with it, never separately.
	relaysByHost = make(map[string]map[uint32]struct{})
	// relayGenerations counts how many times each address has been released. A report is
	// authorised under one generation and claimed under another only if a release happened in
	// between, which is exactly the case a claim must not survive.
	relayGenerations = make(map[string]uint64)
	reportRelaySeq   uint32
	lastRelaySweep   time.Time
)

const (
	// reportRelayLifetime bounds how long the origin of a relayed report is remembered. An
	// SMF that never answers must not cost an entry for the life of the process.
	reportRelayLifetime = 30 * time.Second

	// relaySweepInterval bounds how often the expiry walk runs. Entries are dropped when their
	// answer is sent, so the table holds the reports genuinely in flight and the walk is short --
	// but an SMF that has stopped answering is exactly when it grows, and that is the worst moment
	// to spend a whole-table walk, under the one lock every user plane shares, on every packet.
	// Expiry is housekeeping and does not have to be prompt: what must not be stale is the
	// retransmission match, which tests the age of the entry it matched instead of trusting the
	// sweep to have removed it.
	relaySweepInterval = time.Second

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

// upfAddrReleased is called for an address that has stopped being a user plane the SMF names,
// while the lock that decides that is held. Whoever holds state per peer registers here.
//
// A hook rather than a return value, because the timing is the point: releasing after the lock is
// dropped leaves room for the SMF to name the address again and for a report from it to be
// answered, and the release would then throw away what that answer is holding. Everything that
// authorises a peer goes through this lock, so a release that happens inside it cannot overtake
// one.
var upfAddrReleased func(addr string)

// OnUpfAddrReleased registers the hook. It is not safe to call once the adapter is serving;
// packages register at initialisation.
func OnUpfAddrReleased(hook func(addr string)) {
	upfAddrReleased = hook
}

// ErrRelaySequenceExhausted reports that every sequence number the adapter allocates from is
// outstanding. It is separate from a retransmission because the answer is different: the report
// cannot be relayed at all, and the user plane has to be told so rather than left waiting.
var ErrRelaySequenceExhausted = errors.New("no free relay sequence number")

// ErrPeerReleased reports that the address stopped being a user plane the SMF names between the
// source check and the claim. The report cannot be relayed on its behalf: its claim would outlive
// the authorisation, and an address that is reused would have the next occupant's report met by
// this one's answer.
var ErrPeerReleased = errors.New("the user plane was released while its report was being claimed")

// ErrRelayNotHeld reports that the relay a caller wants renumbered is no longer in the table --
// it was answered, or given up on, while the send that failed was in flight.
var ErrRelayNotHeld = errors.New("no such report relay")

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
func RecordUpfAddr(nodeId *types.NodeID) {
	// Read before resolving, so that it dates the resolution rather than the commit. Two messages
	// naming the same node can be in flight at once -- this is called from concurrent HTTP
	// handlers -- and resolution happens outside the lock, so the one that resolved first can
	// reach the lock second. Without a date on it, a stale answer would be written over a fresh
	// one: the address the node has moved away from would be authorised again, and the address it
	// moved to forgotten.
	now := time.Now()
	ip := nodeId.ResolveNodeIdToIp()
	node := nodeKey(nodeId)

	upfAddrMutex.Lock()
	defer upfAddrMutex.Unlock()

	// This message is the SMF naming the node, so record that before the sweep rather than
	// after it. The other order expires a node whose name has not resolved for longer than the
	// lifetime on the very message that shows it is still in use.
	record := upfNodeAddrs[node]

	superseded := record.seen.After(now)
	if !superseded {
		record.seen = now
		upfNodeAddrs[node] = record
	}

	expireUpfAddrsLocked(now)

	if ip == nil || ip.IsUnspecified() {
		// An FQDN that does not resolve yields IPv4zero, and there is no address to
		// authorise. The identity stays named, because a name that fails to resolve for a
		// moment is not a user plane that has gone: revoking here would stop relaying for a
		// node that is merely waiting on DNS. What the record buys is the other case -- a
		// node the SMF stops naming altogether ages out, and its address with it.
		return
	}

	key := ip.String()

	if superseded {
		// A newer resolution of this node has already been applied. This one describes where the
		// node was, so it authorises nothing and forgets nothing -- it would undo the newer one
		// in both directions.
		logger.CfgLog.Infof("a later message already placed the user plane %s; not applying an older resolution to [%s]",
			node, key)

		return
	}

	// A node that moved stops authorising the address it left. Without this the set only
	// ever grows: a user plane reached through a name, or one that came back on a new
	// address, leaves its old address authorised for the life of the process, and reports
	// from whatever occupies that address next are relayed as if the SMF had named it.
	if record.addr != "" && record.addr != key {
		forgetUpfAddrLocked(node, record.addr)
	}

	if _, known := upfAddrs[key]; !known {
		// The resolved address only: NodeIdValue is four raw octets for an IPv4 node ID,
		// which prints as rubbish.
		logger.CfgLog.Infof("the SMF addresses a user plane at [%s]; reports from it will be relayed", key)
		upfAddrs[key] = make(map[string]struct{})
	}

	upfAddrs[key][node] = struct{}{}
	upfNodeAddrs[node] = upfNodeRecord{addr: key, seen: now}
}

// expireUpfAddrsLocked drops the user planes the SMF has stopped naming, and the addresses left
// with nothing at them. Callers hold upfAddrMutex.
//
// It runs from RecordUpfAddr rather than on a timer because every message the SMF forwards passes
// through there, so the sweep happens exactly as often as there is anything to sweep.
func expireUpfAddrsLocked(now time.Time) {
	for node, record := range upfNodeAddrs {
		if now.Sub(record.seen) <= upfAddrLifetime {
			continue
		}

		delete(upfNodeAddrs, node)

		if record.addr != "" {
			logger.CfgLog.Infof("the SMF has not named the user plane at [%s] for %s; it is no longer relayed for",
				record.addr, upfAddrLifetime)

			forgetUpfAddrLocked(node, record.addr)
		}
	}
}

// nodeKey identifies one user plane across address changes. The type is part of it because
// the same octets mean different things under different node id types.
func nodeKey(nodeId *types.NodeID) string {
	return fmt.Sprintf("%d/%s", nodeId.NodeIdType, nodeId.NodeIdValue)
}

// forgetUpfAddrLocked drops one identity from an address, and the address with it once no
// identity is left there -- releasing what anyone else was holding for that address through the
// upfAddrReleased hook, from inside the critical section rather than after it. Callers hold
// upfAddrMutex.
func forgetUpfAddrLocked(node, addr string) {
	nodes, known := upfAddrs[addr]
	if !known {
		return
	}

	delete(nodes, node)

	if len(nodes) > 0 {
		return
	}

	delete(upfAddrs, addr)
	logger.CfgLog.Infof("no user plane the SMF has named is at [%s] any more; reports from it will not be relayed", addr)

	ForgetRelaysForHost(addr)

	if upfAddrReleased != nil {
		upfAddrReleased(addr)
	}
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

// RelayGeneration reports how many times this address has been released, for a caller to hand back
// when it claims. Read it before testing whether the address is authorised: a release between the
// two is what this exists to catch, and one before the read is caught by the test itself.
func RelayGeneration(addr string) uint64 {
	reportRelayMutex.Lock()
	defer reportRelayMutex.Unlock()

	return relayGenerations[addr]
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
func RelayReportSequence(upfAddr *net.UDPAddr, upfSeq uint32, now time.Time, authorisedUnder uint64) (relaySeq uint32, fresh bool, err error) {
	reportRelayMutex.Lock()
	defer reportRelayMutex.Unlock()

	// The source check that let this report through released its lock before this claim was made,
	// and an SMF message can release the address in that gap. Claiming anyway would leave a claim
	// for a peer the adapter no longer relays for -- and if the address is reused, the next
	// occupant's report matches it as a retransmission and is answered with this one's response.
	if relayGenerations[upfAddr.IP.String()] != authorisedUnder {
		return 0, false, ErrPeerReleased
	}

	sweepRelaysLocked(now)

	if outstanding, relaying := relayFromLocked(upfAddr, upfSeq, now); relaying {
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
	recordRelayLocked(candidate, reportRelay{upfAddr: upfAddr, upfSeq: upfSeq, recorded: now})

	return candidate, true, nil
}

// relayFromLocked reports the number a report from this peer is already being relayed under.
//
// The entry's age is tested here rather than left to the sweep: the sweep is throttled, so an
// entry that has outlived the lifetime can still be in the table, and matching it would answer a
// fresh report with a relay that is no longer waiting for anything.
func relayFromLocked(upfAddr *net.UDPAddr, upfSeq uint32, now time.Time) (uint32, bool) {
	for held := range relaysByHost[upfAddr.IP.String()] {
		relay := reportRelays[held]
		if relay.upfSeq == upfSeq && relay.upfAddr.Port == upfAddr.Port &&
			now.Sub(relay.recorded) <= reportRelayLifetime {
			return held, true
		}
	}

	return 0, false
}

// recordRelayLocked and dropRelayLocked are the only writers of the two tables, so the index
// cannot drift from what it indexes.
func recordRelayLocked(relaySeq uint32, relay reportRelay) {
	reportRelays[relaySeq] = relay

	host := relay.upfAddr.IP.String()
	if relaysByHost[host] == nil {
		relaysByHost[host] = make(map[uint32]struct{})
	}

	relaysByHost[host][relaySeq] = struct{}{}
}

func dropRelayLocked(relaySeq uint32) {
	relay, held := reportRelays[relaySeq]
	if !held {
		return
	}

	delete(reportRelays, relaySeq)

	host := relay.upfAddr.IP.String()
	delete(relaysByHost[host], relaySeq)

	if len(relaysByHost[host]) == 0 {
		delete(relaysByHost, host)
	}
}

// sweepRelaysLocked drops the entries the SMF never answered, at most once every
// relaySweepInterval.
func sweepRelaysLocked(now time.Time) {
	if now.Sub(lastRelaySweep) < relaySweepInterval {
		return
	}

	lastRelaySweep = now

	for held, relay := range reportRelays {
		if now.Sub(relay.recorded) <= reportRelayLifetime {
			continue
		}

		// Both numbers, and the peer: the table is keyed by the adapter's own sequence while the
		// user plane is waiting on its own, and several user planes can be waiting on the same
		// one. Reporting only the user plane's number names an entry that cannot be looked up.
		logger.CfgLog.Warnf("no response was relayed for session report from %v: adapter seq[%d], user plane seq[%d]; forgetting it",
			relay.upfAddr, held, relay.upfSeq)

		dropRelayLocked(held)
	}
}

// RenumberReportRelay moves a relay that could not go out under its number to the next free one,
// keeping the report's identity reserved throughout.
//
// The number is refused when an SMF-originated request is already in flight under it, and the
// report has to be sent under another. Dropping the entry and allocating again left the report
// unclaimed in between: a retransmission arriving in that gap matched nothing, was taken for a
// fresh report, and was relayed on its own account -- so one report from one user plane became
// two downlink data notifications, which is what the freshness check exists to prevent. Messages
// are dispatched on their own goroutines, so the gap is reachable.
func RenumberReportRelay(relaySeq uint32) (uint32, error) {
	reportRelayMutex.Lock()
	defer reportRelayMutex.Unlock()

	relay, held := reportRelays[relaySeq]
	if !held {
		return 0, ErrRelayNotHeld
	}

	candidate, free := nextFreeRelaySequence(reportRelays, reportRelaySeq, relaySequenceFloor, relaySequenceCeil)
	if !free {
		// The claim stays. Releasing it here would end it before the caller has answered the user
		// plane, and between those two moments a retransmission is recognised by neither the claim
		// nor a response transaction -- so it would be relayed afresh. The caller ends the claim
		// after its refusal is registered.
		return 0, ErrRelaySequenceExhausted
	}

	dropRelayLocked(relaySeq)

	reportRelaySeq = candidate
	recordRelayLocked(candidate, relay)

	return candidate, nil
}

// ForgetRelaysForHost drops the reports being relayed for a peer that has stopped being one the
// SMF names. Callers hold upfAddrMutex; this takes reportRelayMutex under it, which is the only
// order the two are ever held in.
//
// Without it the origins outlived the authorisation by up to reportRelayLifetime. An address in a
// cluster is reused, and the next occupant's first report can carry a sequence number the previous
// occupant had outstanding: it would be matched as that one's retransmission, so the new peer's
// report is never relayed and the answer to the old one is delivered to it instead.
func ForgetRelaysForHost(addr string) {
	reportRelayMutex.Lock()
	defer reportRelayMutex.Unlock()

	// Through dropRelayLocked, one entry at a time, rather than clearing the two tables here:
	// deleting from a map while ranging over it is safe, and a second place that writes them
	// would be a second place to remember when either grows a field. The bucket goes with its
	// last entry.
	// Counted whether or not anything was held: what a claim needs to know is that a release
	// happened, not that it found something to drop.
	relayGenerations[addr]++

	for held := range relaysByHost[addr] {
		logger.CfgLog.Infof("user plane at [%s] is no longer named by the SMF; forgetting its session report relay: adapter seq[%d], user plane seq[%d]",
			addr, held, reportRelays[held].upfSeq)

		dropRelayLocked(held)
	}
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

	// Age is tested here and not left to the sweep, which is throttled and may not have reached
	// this entry yet. The lifetime is what the user plane was promised: past it, the adapter has
	// given up on the report and the user plane has been told so, and answering it afterwards
	// would answer a report nothing is waiting on.
	if time.Since(relay.recorded) > reportRelayLifetime {
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

	dropRelayLocked(relaySeq)
}

func InsertUpfPfcpTxn(seq uint32, pfcpTxnChan PfcpTxnChan) {
	logger.CfgLog.Debugf(" inserting transaction with sequence number [%v]", seq)
	UpfTxnsMutex.Lock()
	UpfTxns[seq] = pfcpTxnChan
	UpfTxnsMutex.Unlock()
}

// ForgetUpfPfcpTxn drops a registration whose request never went out. The entry is made before the
// send, because a response can arrive before the send call returns; when the send fails there is
// nothing to wait for it, and an entry left behind hands the next response carrying that sequence
// number to a requester that has already gone.
//
// It drops its own registration and only its own. Sequence numbers are the SMF's, and a second
// request can carry one that is already in flight here -- a retransmission does so by definition.
// Deleting by number alone would then take that request's registration instead, and the very fault
// this repairs would happen to it: its answer arrives, finds nothing waiting, and is dropped while
// it waits for one that was given away.
func ForgetUpfPfcpTxn(seq uint32, own PfcpTxnChan) {
	UpfTxnsMutex.Lock()
	defer UpfTxnsMutex.Unlock()

	if held, ok := UpfTxns[seq]; !ok || held != own {
		return
	}

	delete(UpfTxns, seq)
	logger.CfgLog.Debugf("dropped the transaction with sequence number [%v]: its request did not go out", seq)
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
