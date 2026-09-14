// SPDX-FileCopyrightText: 2026 Forsway Scandinavia AB
//
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"net"
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
	probe, _ := config.RelayReportSequence(upfAddr, 0xFFFF, time.Now())
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
