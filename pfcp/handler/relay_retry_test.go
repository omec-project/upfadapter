// SPDX-FileCopyrightText: 2026 Forsway Scandinavia AB
//
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omec-project/upfadapter/config"
	"github.com/omec-project/upfadapter/pfcp/udp"
	"github.com/omec-project/upfadapter/types"
	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"
)

// listeningAdapter gives the handler a socket to send from. It is deliberately not torn
// down: a relayed request keeps a transaction goroutine alive for its resend window, and
// clearing udp.Server underneath it would be a data race of the test's own making.
func listeningAdapter(t *testing.T) {
	t.Helper()

	if udp.Server != nil && udp.Server.Conn != nil {
		return
	}

	packet, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	conn, ok := packet.(*net.UDPConn)
	if !ok {
		t.Fatalf("listener is %T, want *net.UDPConn", packet)
	}

	udp.Server = &udp.PfcpServer{Addr: conn.LocalAddr().(*net.UDPAddr), Conn: conn}
}

// Requests the adapter sends share one transaction table, keyed by its own socket, so a
// sequence number the SMF chose and one the adapter chose meet there. The adapter cannot
// see the SMF's counter, and starting its own at the halfway point only keeps them apart
// until that counter arrives -- after which a report would be rejected for a coincidence.
// It tries the next number instead.
func TestASessionReportIsRelayedUnderTheNextNumberWhenOneIsInFlight(t *testing.T) {
	listeningAdapter(t)

	upfAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.2"), Port: config.PfcpPort}
	config.RecordUpfAddr(types.NewNodeID("127.0.0.2"))
	config.SetSmfAddr("127.0.0.1")

	// Learn where the allocator has got to, so the test does not depend on being the first
	// to relay anything in this process.
	probe, _, err := config.RelayReportSequence(upfAddr, 0xFFFF, time.Now())
	if err != nil {
		t.Fatalf("probing the allocator: %v", err)
	}

	config.ForgetReportRelay(probe)

	taken, free := probe+1, probe+2

	inFlight := message.NewSessionReportRequest(0, 0, 0x1234, taken, 0,
		ie.NewReportType(0, 1, 0, 0))
	buf := make([]byte, inFlight.MarshalLen())

	if err := inFlight.MarshalTo(buf); err != nil {
		t.Fatalf("marshal: %v", err)
	}

	smfAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: config.PfcpPort}
	if err := udp.PutTransaction(udp.NewTransaction(inFlight, buf, udp.Server.Conn, smfAddr, nil)); err != nil {
		t.Fatalf("standing in for a request the SMF numbered: %v", err)
	}

	report := message.NewSessionReportRequest(0, 0, 0x5678, 11, 0, ie.NewReportType(0, 1, 0, 0))

	HandlePfcpSessionReportRequest(report, upfAddr)

	addr, upfSeq := config.TakeReportRelay(free)
	if addr == nil {
		t.Fatalf("no relay recorded under seq[%d]: the report was rejected rather than relayed under the next number", free)
	}

	t.Cleanup(func() { config.ForgetReportRelay(free) })

	if !addr.IP.Equal(upfAddr.IP) || upfSeq != 11 {
		t.Errorf("relay under seq[%d] came from %v seq[%d], want %v seq[11]", free, addr, upfSeq, upfAddr)
	}
}

// refusalTestSequences hands out sequence numbers no run of the refusal test has used before, so
// runs in one process do not meet each other's answered reports: an answered report is held for
// its resend window, and a reused number would be refused as its duplicate.
var refusalTestSequences atomic.Uint32

func init() { refusalTestSequences.Store(3000) }

// absorbedByAnAnswer reports whether a retransmitted report would be met by the answer already
// sent for it, which is what the receive path asks before a report ever reaches this handler.
func absorbedByAnAnswer(upfAddr *net.UDPAddr, upfSeq uint32) bool {
	txTable, held := udp.Server.ConsumerTable.Load(upfAddr.String())
	if !held {
		return false
	}

	_, answered := txTable.Load(upfSeq)

	return answered
}

// Refusing a report hands it from one absorber to another: its claim, which recognises a
// retransmission while the report is being relayed, and the answer, which meets one once the
// report has been refused. What this pins is that the handover completes -- the answer is
// registered and the claim is gone -- so a retransmission after a refusal is met by the answer.
//
// It does not pin the order of those two, and no test here can. A sampler asking the two questions
// the receive path asks has its own gap between them: it can read "no answer" before the refusal
// is sent and "no claim" after it has been released, and report a window that never existed. An
// earlier version of this test did exactly that, about once in a hundred rounds, and the ordering
// it appeared to prove was an artefact. The ordering is held by refuseAndRelease being the one
// place that ends a claim, with its reason written there.
func TestRefusingAReportHandsItToItsAnswer(t *testing.T) {
	listeningAdapter(t)

	upfSocket, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the user plane: %v", err)
	}

	defer upfSocket.Close()

	upfAddr, ok := upfSocket.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("user plane listener is %T, want *net.UDPAddr", upfSocket.LocalAddr())
	}

	upfSeq := refusalTestSequences.Add(1)

	relaySeq, fresh, err := config.RelayReportSequence(upfAddr, upfSeq, time.Now())
	if err != nil || !fresh {
		t.Fatalf("claiming the report: relay %d fresh %v err %v", relaySeq, fresh, err)
	}

	refuseAndRelease(0x9999, upfSeq, upfAddr, relaySeq)

	if !absorbedByAnAnswer(upfAddr, upfSeq) {
		t.Error("the refusal left no answer to meet a retransmission of the report")
	}

	if addr, _ := config.TakeReportRelay(relaySeq); addr != nil {
		t.Error("the claim outlived the refusal, so the report is held by both and released by neither")
	}
}
