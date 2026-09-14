// SPDX-FileCopyrightText: 2026 Forsway Scandinavia AB
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"net"
	"testing"
	"time"

	"github.com/omec-project/upfadapter/types"
)

// withSmfAddr isolates the package-level SMF address so ordering between tests cannot
// decide their outcome.
func withSmfAddr(t *testing.T, value net.IP) {
	t.Helper()

	smfAddrMutex.Lock()
	previous := smfAddr
	smfAddr = value
	smfAddrMutex.Unlock()

	t.Cleanup(func() {
		smfAddrMutex.Lock()
		smfAddr = previous
		smfAddrMutex.Unlock()
	})
}

func TestSmfAddrUnknownUntilAnSmfSpeaks(t *testing.T) {
	withSmfAddr(t, nil)

	if addr := SmfAddr(); addr != nil {
		t.Fatalf("SmfAddr() = %v, want nil before any SMF message", addr)
	}
}

func TestSetSmfAddrRecordsWhereToRelay(t *testing.T) {
	withSmfAddr(t, nil)

	SetSmfAddr("10.42.0.188")

	addr := SmfAddr()
	if addr == nil {
		t.Fatal("SmfAddr() = nil, want the recorded address")
	}

	if !addr.IP.Equal(net.ParseIP("10.42.0.188")) || addr.Port != PfcpPort {
		t.Errorf("SmfAddr() = %v, want 10.42.0.188:%d", addr, PfcpPort)
	}
}

// An SMF-initiated message without the field must not erase an address we already have,
// or one malformed request would stop every later report from being relayed.
func TestSetSmfAddrIgnoresEmpty(t *testing.T) {
	withSmfAddr(t, net.ParseIP("10.42.0.188"))

	SetSmfAddr("")

	if addr := SmfAddr(); addr == nil || !addr.IP.Equal(net.ParseIP("10.42.0.188")) {
		t.Errorf("SmfAddr() = %v, want the previously recorded address", addr)
	}
}

// The address is claimed in an unauthenticated request body. A claim that is not an IP
// must be refused where it arrives: stored unparsed it erased an address that worked, and
// every report was then rejected until the next SMF message happened to carry a good one.
func TestSetSmfAddrRefusesAClaimThatIsNotAnIP(t *testing.T) {
	withSmfAddr(t, net.ParseIP("10.42.0.188"))

	SetSmfAddr("upf-adapter.local")

	if addr := SmfAddr(); addr == nil || !addr.IP.Equal(net.ParseIP("10.42.0.188")) {
		t.Errorf("SmfAddr() = %v, want the previously recorded address", addr)
	}

	withSmfAddr(t, nil)

	SetSmfAddr("upf-adapter.local")

	if addr := SmfAddr(); addr != nil {
		t.Errorf("SmfAddr() = %v, want nil for a claim that is not an IP", addr)
	}
}

// The recorded address is handed out while the package keeps writing the field, so what
// a caller gets must not be the stored slice itself.
func TestSmfAddrReturnsACopy(t *testing.T) {
	withSmfAddr(t, net.ParseIP("10.42.0.188"))

	addr := SmfAddr()
	addr.IP[len(addr.IP)-1] = 0

	if again := SmfAddr(); again == nil || !again.IP.Equal(net.ParseIP("10.42.0.188")) {
		t.Errorf("SmfAddr() = %v after a caller wrote to its address, want 10.42.0.188", again)
	}
}

// withReportRelays isolates the relay table and the sequence counter, so a test that
// leaves an entry behind -- or stops at a Fatalf before taking one -- cannot decide
// another test's outcome.
func withReportRelays(t *testing.T) {
	t.Helper()

	reportRelayMutex.Lock()
	relays, seq := reportRelays, reportRelaySeq
	reportRelays, reportRelaySeq = make(map[uint32]reportRelay), 0
	reportRelayMutex.Unlock()

	t.Cleanup(func() {
		reportRelayMutex.Lock()
		reportRelays, reportRelaySeq = relays, seq
		reportRelayMutex.Unlock()
	})
}

// A relayed report is renumbered into the adapter's own space, and the answer must carry
// the number the user plane is waiting for -- not the adapter's.
func TestRelayReportSequenceKeepsTheUpfsOwnNumber(t *testing.T) {
	withReportRelays(t)

	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.184"), Port: PfcpPort}

	relaySeq, _ := RelayReportSequence(upfAddr, 4711, time.Now())
	if relaySeq == 4711 {
		t.Error("the report was relayed under the UPF's own sequence number, which shares a table with the SMF's")
	}

	if relaySeq < relaySequenceFloor || relaySeq > relaySequenceCeil {
		t.Errorf("relay sequence %#x outside the adapter's range [%#x, %#x]",
			relaySeq, relaySequenceFloor, relaySequenceCeil)
	}

	addr, upfSeq := TakeReportRelay(relaySeq)
	if addr == nil || !addr.IP.Equal(upfAddr.IP) || addr.Port != upfAddr.Port {
		t.Fatalf("TakeReportRelay(%d) address = %v, want %v", relaySeq, addr, upfAddr)
	}

	if upfSeq != 4711 {
		t.Errorf("TakeReportRelay(%d) UPF sequence = %d, want 4711", relaySeq, upfSeq)
	}

	// Taken exactly once, so a duplicate response cannot be forwarded to a stale peer.
	if again, _ := TakeReportRelay(relaySeq); again != nil {
		t.Errorf("TakeReportRelay(%d) second call = %v, want nil", relaySeq, again)
	}
}

// Two user planes number their reports independently, so the same number arriving from
// both must still resolve to the right peer.
func TestRelayReportSequenceSeparatesTwoUpfsUsingTheSameNumber(t *testing.T) {
	withReportRelays(t)

	first := &net.UDPAddr{IP: net.ParseIP("10.42.0.184"), Port: PfcpPort}
	second := &net.UDPAddr{IP: net.ParseIP("10.42.0.185"), Port: PfcpPort}

	now := time.Now()

	firstRelay, firstFresh := RelayReportSequence(first, 1, now)
	secondRelay, secondFresh := RelayReportSequence(second, 1, now)

	// Both are reports in their own right. Recognising a retransmission by its sequence
	// number alone would make the second one the first one arriving twice.
	if !firstFresh || !secondFresh {
		t.Fatalf("reports from two user planes were taken for one: fresh = %v and %v, want both true", firstFresh, secondFresh)
	}

	if firstRelay == secondRelay {
		t.Fatalf("both reports were relayed as seq[%d]; one origin has been lost", firstRelay)
	}

	firstAddr, firstSeq := TakeReportRelay(firstRelay)
	secondAddr, secondSeq := TakeReportRelay(secondRelay)

	if firstAddr == nil || !firstAddr.IP.Equal(first.IP) {
		t.Errorf("first report resolved to %v, want %v", firstAddr, first)
	}

	if secondAddr == nil || !secondAddr.IP.Equal(second.IP) {
		t.Errorf("second report resolved to %v, want %v", secondAddr, second)
	}

	if firstSeq != 1 || secondSeq != 1 {
		t.Errorf("restored UPF sequences = %d and %d, want 1 and 1", firstSeq, secondSeq)
	}
}

// A user-plane function retransmits a request it has not been answered. Until the answer
// exists there is no response transaction holding the report, so nothing else recognises
// the copy: relaying it as a report of its own raises a second downlink data notification
// for traffic the SMF is already being told about.
func TestRelayReportSequenceRecognisesARetransmission(t *testing.T) {
	withReportRelays(t)

	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.187"), Port: PfcpPort}

	now := time.Now()

	first, fresh := RelayReportSequence(upfAddr, 7, now)
	if !fresh {
		t.Fatal("the first report was taken for a retransmission")
	}

	again, fresh := RelayReportSequence(upfAddr, 7, now.Add(3*time.Second))
	if fresh {
		t.Error("a retransmission was relayed as a report of its own")
	}

	if again != first {
		t.Errorf("retransmission resolved to seq[%d], want the outstanding seq[%d]", again, first)
	}

	// The next report is numbered as though the retransmission had never arrived, which is
	// what says no second entry was made for it.
	other, fresh := RelayReportSequence(upfAddr, 8, now)
	if !fresh || other != first+1 {
		t.Errorf("the next report = seq[%d] (fresh = %v), want seq[%d]: the retransmission consumed a sequence number",
			other, fresh, first+1)
	}

	TakeReportRelay(other)
	ForgetReportRelay(other)

	if addr, _ := TakeReportRelay(first); addr == nil {
		t.Fatalf("TakeReportRelay(%d) = nil, want the outstanding relay", first)
	}

	ForgetReportRelay(first)

	// Once the answer has been sent and the exchange forgotten, the same number is a new
	// report again -- the user plane's counter comes round, and this must not make it
	// unreachable. Taking alone does not end it: see
	// TestTakeReportRelayKeepsTheEntryUntilItIsForgotten for what the entry is still doing
	// between the two.
	repeated, fresh := RelayReportSequence(upfAddr, 7, now)
	if !fresh {
		t.Error("a report reusing the number of an exchange already answered was refused as a retransmission")
	}

	TakeReportRelay(repeated)
	ForgetReportRelay(repeated)
}

// A peer is an address and a port -- that is the identity every other transaction in the
// adapter is keyed by -- so two user planes behind one address are two peers, and a report
// from each is a report of its own.
func TestRelayReportSequenceSeparatesTwoPortsAtOneAddress(t *testing.T) {
	withReportRelays(t)

	first := &net.UDPAddr{IP: net.ParseIP("10.42.0.189"), Port: PfcpPort}
	second := &net.UDPAddr{IP: net.ParseIP("10.42.0.189"), Port: PfcpPort + 1}

	now := time.Now()

	firstRelay, firstFresh := RelayReportSequence(first, 1, now)

	secondRelay, secondFresh := RelayReportSequence(second, 1, now)
	if !firstFresh || !secondFresh {
		t.Fatalf("reports from two ports were taken for one: fresh = %v and %v, want both true", firstFresh, secondFresh)
	}

	if firstRelay == secondRelay {
		t.Fatalf("both reports were relayed as seq[%d]; one origin has been lost", firstRelay)
	}

	addr, _ := TakeReportRelay(secondRelay)
	if addr == nil || addr.Port != second.Port {
		t.Errorf("second report resolved to %v, want %v", addr, second)
	}

	TakeReportRelay(firstRelay)
}

// An entry past its lifetime is forgotten, not read as a relay still in flight: the user
// plane would otherwise never have that report relayed at all.
func TestRelayReportSequenceRelaysAgainOnceTheOutstandingEntryExpired(t *testing.T) {
	withReportRelays(t)

	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.188"), Port: PfcpPort}

	now := time.Now()

	stale, _ := RelayReportSequence(upfAddr, 11, now.Add(-2*reportRelayLifetime))

	relaySeq, fresh := RelayReportSequence(upfAddr, 11, now)
	if !fresh {
		t.Error("a report whose outstanding entry had expired was taken for a retransmission")
	}

	if relaySeq == stale {
		t.Errorf("the expired entry was handed back as seq[%d]", stale)
	}

	TakeReportRelay(relaySeq)
}

// Sequence numbers are three octets on the wire, so the counter has to come back round
// inside the adapter's own range rather than overflow into the SMF's.
func TestRelayReportSequenceWrapsWithinItsOwnRange(t *testing.T) {
	withReportRelays(t)

	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.184"), Port: PfcpPort}

	reportRelayMutex.Lock()
	reportRelaySeq = relaySequenceCeil
	reportRelayMutex.Unlock()

	relaySeq, _ := RelayReportSequence(upfAddr, 7, time.Now())
	if relaySeq != relaySequenceFloor {
		t.Errorf("sequence after the ceiling = %#x, want %#x", relaySeq, relaySequenceFloor)
	}

	TakeReportRelay(relaySeq)
}

// An SMF that never answers must not cost an entry for the life of the process.
func TestReportRelayForgetsEntriesTheSmfNeverAnswered(t *testing.T) {
	withReportRelays(t)

	stale := &net.UDPAddr{IP: net.ParseIP("10.42.0.184"), Port: PfcpPort}
	fresh := &net.UDPAddr{IP: net.ParseIP("10.42.0.185"), Port: PfcpPort}

	now := time.Now()

	staleRelay, _ := RelayReportSequence(stale, 1, now.Add(-2*reportRelayLifetime))
	freshRelay, _ := RelayReportSequence(fresh, 2, now)

	if addr, _ := TakeReportRelay(staleRelay); addr != nil {
		t.Errorf("TakeReportRelay(%d) = %v, want nil for an entry past its lifetime", staleRelay, addr)
	}

	if addr, _ := TakeReportRelay(freshRelay); addr == nil {
		t.Errorf("TakeReportRelay(%d) = nil, want the entry recorded just now", freshRelay)
	}
}

func TestTakeReportRelayUnknownSequence(t *testing.T) {
	withReportRelays(t)

	if addr, _ := TakeReportRelay(999999); addr != nil {
		t.Errorf("TakeReportRelay(unknown) = %v, want nil", addr)
	}
}

// isolateUpfAddrs empties the recorded user planes so ordering between tests cannot
// decide their outcome. Both directions of the index are replaced: leaving the reverse one
// behind would let a node id recorded by an earlier test decide where this one thinks it
// moved from.
func isolateUpfAddrs(t *testing.T) {
	t.Helper()

	upfAddrMutex.Lock()
	previousAddrs, previousNodes := upfAddrs, upfNodeAddrs
	upfAddrs = make(map[string]map[string]struct{})
	upfNodeAddrs = make(map[string]string)
	upfAddrMutex.Unlock()

	t.Cleanup(func() {
		upfAddrMutex.Lock()
		upfAddrs, upfNodeAddrs = previousAddrs, previousNodes
		upfAddrMutex.Unlock()
	})
}

// Before the SMF has named any user plane there is nothing to relay for, and a report
// from an unknown source must not be taken on trust.
func TestNoUpfIsKnownBeforeTheSmfNamesOne(t *testing.T) {
	isolateUpfAddrs(t)

	if IsKnownUpfAddr(net.ParseIP("10.42.0.184")) {
		t.Error("IsKnownUpfAddr() = true with nothing recorded, want false")
	}
}

func TestRecordUpfAddrByIP(t *testing.T) {
	isolateUpfAddrs(t)

	RecordUpfAddr(types.NewNodeID("10.42.0.184"))

	if !IsKnownUpfAddr(net.ParseIP("10.42.0.184")) {
		t.Error("IsKnownUpfAddr() = false for the user plane the SMF named, want true")
	}

	// The peer address read off the socket is a 4-byte IPv4 where the node ID resolved to
	// a 16-byte one; the two must still be the same user plane.
	if !IsKnownUpfAddr(net.ParseIP("10.42.0.184").To4()) {
		t.Error("IsKnownUpfAddr() = false for the same address in 4-byte form, want true")
	}
}

// Anything else that can reach the port is refused, which is the whole point: N4 has no
// transport authentication, so the SMF naming a user plane is the only thing that makes
// it one.
func TestIsKnownUpfAddrRefusesAnySourceTheSmfDidNotName(t *testing.T) {
	isolateUpfAddrs(t)

	RecordUpfAddr(types.NewNodeID("10.42.0.184"))

	if IsKnownUpfAddr(net.ParseIP("10.42.0.185")) {
		t.Error("IsKnownUpfAddr() = true for a source the SMF never named, want false")
	}

	if IsKnownUpfAddr(nil) {
		t.Error("IsKnownUpfAddr(nil) = true, want false")
	}
}

// A user plane configured by name is matched by the address it actually sends from, not
// by the name, because the peer address is all a report carries.
func TestRecordUpfAddrResolvesAnFqdn(t *testing.T) {
	isolateUpfAddrs(t)

	types.InsertDnsHostIp("upf.5gc.svc", net.ParseIP("10.42.0.190"))

	RecordUpfAddr(types.NewNodeID("upf.5gc.svc"))

	if !IsKnownUpfAddr(net.ParseIP("10.42.0.190")) {
		t.Error("IsKnownUpfAddr() = false for the address an FQDN-configured user plane resolves to, want true")
	}
}

// An unresolvable name yields IPv4zero, and recording that would admit an address no
// real peer has. The value is driven in directly rather than through a name that does
// not resolve, so the test does not depend on a DNS lookup failing.
func TestRecordUpfAddrIgnoresAnUnspecifiedAddress(t *testing.T) {
	isolateUpfAddrs(t)

	RecordUpfAddr(&types.NodeID{NodeIdType: types.NodeIdTypeIpv4Address, NodeIdValue: net.IPv4zero.To4()})

	if IsKnownUpfAddr(net.IPv4zero) {
		t.Error("IsKnownUpfAddr(0.0.0.0) = true, want false")
	}
}

// 0.0.0.0 and :: are IP addresses and not destinations. A claim carrying one used to be
// stored like any other, and every report relayed afterwards went to an address that
// answers nothing -- until some later SMF message happened to carry a usable one.
func TestSetSmfAddrRefusesTheUnspecifiedAddress(t *testing.T) {
	working := net.ParseIP("10.10.0.5")

	for _, claim := range []string{"0.0.0.0", "::"} {
		withSmfAddr(t, working)

		SetSmfAddr(claim)

		addr := SmfAddr()
		if addr == nil || !addr.IP.Equal(working) {
			t.Errorf("after a claimed SMF address of %q, SmfAddr() = %v, want the working address %v",
				claim, addr, working)
		}
	}
}

// A user plane reached by name can move: the pod comes back at another address, and the
// name now resolves there. The address it left must stop authorising reports, or an
// unrelated peer that takes that address over is relayed for as though the SMF had named
// it -- and the set grows for the life of the process.
func TestRecordUpfAddrForgetsTheAddressANodeLeft(t *testing.T) {
	isolateUpfAddrs(t)

	types.InsertDnsHostIp("moving.5gc.svc", net.ParseIP("10.42.0.200"))
	RecordUpfAddr(types.NewNodeID("moving.5gc.svc"))

	types.InsertDnsHostIp("moving.5gc.svc", net.ParseIP("10.42.0.201"))
	RecordUpfAddr(types.NewNodeID("moving.5gc.svc"))

	if IsKnownUpfAddr(net.ParseIP("10.42.0.200")) {
		t.Error("IsKnownUpfAddr() = true for the address the user plane left, want false")
	}

	if !IsKnownUpfAddr(net.ParseIP("10.42.0.201")) {
		t.Error("IsKnownUpfAddr() = false for the address the user plane moved to, want true")
	}
}

// One address can carry two identities -- the SMF may name the same user plane by FQDN in
// one message and by IP in another -- so an address is forgotten only once nothing is left
// at it. Forgetting on the first move would revoke a peer that is still there.
func TestRecordUpfAddrKeepsAnAddressAnotherIdentityStillUses(t *testing.T) {
	isolateUpfAddrs(t)

	shared := net.ParseIP("10.42.0.210")

	types.InsertDnsHostIp("shared.5gc.svc", shared)
	RecordUpfAddr(types.NewNodeID("shared.5gc.svc"))
	RecordUpfAddr(types.NewNodeID("10.42.0.210"))

	types.InsertDnsHostIp("shared.5gc.svc", net.ParseIP("10.42.0.211"))
	RecordUpfAddr(types.NewNodeID("shared.5gc.svc"))

	if !IsKnownUpfAddr(shared) {
		t.Error("IsKnownUpfAddr() = false for an address the SMF still names by IP, want true")
	}
}

// Answering a report does not end the adapter's need to recognise copies of it. The
// response transaction that absorbs them exists only once the answer has been sent, and
// messages are handled on their own goroutines, so an entry deleted at the moment of
// taking leaves a window in which a retransmission is relayed a second time -- a second
// downlink data notification, and a second page.
func TestTakeReportRelayKeepsTheEntryUntilItIsForgotten(t *testing.T) {
	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.220"), Port: PfcpPort}
	now := time.Now()

	relaySeq, fresh := RelayReportSequence(upfAddr, 7, now)
	if !fresh {
		t.Fatalf("precondition: the first relay of a report must be fresh")
	}

	if addr, _ := TakeReportRelay(relaySeq); addr == nil {
		t.Fatalf("TakeReportRelay(%d) = nil, want the origin of the report", relaySeq)
	}

	if _, fresh := RelayReportSequence(upfAddr, 7, now); fresh {
		t.Error("a retransmission arriving while the answer is being sent was treated as a new report; " +
			"it would be relayed again and raise a second notification")
	}

	ForgetReportRelay(relaySeq)

	if _, fresh := RelayReportSequence(upfAddr, 7, now); !fresh {
		t.Error("a report arriving after the exchange was forgotten was not treated as new")
	}
}

// The SMF's answer and the give-up path both end the same exchange. Only one of them may
// answer the user plane, so the second taker is told there is nothing to take.
func TestTakeReportRelayIsTakeOnce(t *testing.T) {
	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.221"), Port: PfcpPort}

	relaySeq, _ := RelayReportSequence(upfAddr, 9, time.Now())
	t.Cleanup(func() { ForgetReportRelay(relaySeq) })

	if addr, _ := TakeReportRelay(relaySeq); addr == nil {
		t.Fatalf("precondition: the first take must return the origin")
	}

	if addr, _ := TakeReportRelay(relaySeq); addr != nil {
		t.Errorf("TakeReportRelay(%d) = %v on the second take, want nil: both paths would answer the same report",
			relaySeq, addr)
	}
}
