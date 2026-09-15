// SPDX-FileCopyrightText: 2026 Forsway Scandinavia AB
//
// SPDX-License-Identifier: Apache-2.0

package udp

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"
)

// The primitive, asserted directly, because the concurrent test below can only catch the race
// when the scheduler interleaves the two senders: against the previous Load-then-Store shape it
// failed 13 runs in 200 by default and 63 in 200 under -race. This one fails every time.
func TestTxTableRefusesASecondSenderUnderOneSequence(t *testing.T) {
	table := &TxTable{}
	first := &Transaction{SequenceNumber: 7}
	second := &Transaction{SequenceNumber: 7}

	if _, loaded := table.LoadOrStore(7, first); loaded {
		t.Fatal("the first sender was told the sequence was taken")
	}

	existing, loaded := table.LoadOrStore(7, second)
	if !loaded {
		t.Error("the second sender was allowed to insert under a sequence already in flight")
	}

	if existing != first {
		t.Error("the second sender replaced the transaction whose response is still to come")
	}
}

// Two goroutines numbering requests at once must not both believe they own one sequence. The
// check and the insertion used to be separate operations on the same map, so both could find it
// free and the second would replace a transaction whose response was still to come -- while its
// caller was told the message had gone out. This is a race test and catches that intermittently;
// TestTxTableRefusesASecondSenderUnderOneSequence is the deterministic half.
func TestPutTransactionAdmitsOneSenderPerSequence(t *testing.T) {
	packet, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer packet.Close()

	conn, ok := packet.(*net.UDPConn)
	if !ok {
		t.Fatalf("listener is %T, want *net.UDPConn", packet)
	}

	previous := Server
	Server = &PfcpServer{Addr: conn.LocalAddr().(*net.UDPAddr), Conn: conn}

	t.Cleanup(func() { Server = previous })

	dest := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: PFCP_PORT}

	const senders = 8

	var (
		wg        sync.WaitGroup
		accepted  atomic.Int32
		duplicate atomic.Int32
	)

	for range senders {
		wg.Add(1)

		go func() {
			defer wg.Done()

			msg := message.NewHeartbeatRequest(0x424242, ie.NewRecoveryTimeStamp(time.Unix(0, 0)), nil)

			buf := make([]byte, msg.MarshalLen())
			if err := msg.MarshalTo(buf); err != nil {
				return
			}

			switch err := PutTransaction(NewTransaction(msg, buf, conn, dest, nil)); {
			case err == nil:
				accepted.Add(1)
			case errors.Is(err, ErrDuplicateSequence):
				duplicate.Add(1)
			}
		}()
	}

	wg.Wait()

	if got := accepted.Load(); got != 1 {
		t.Errorf("senders that inserted under one sequence = %d, want 1: the rest must be told it is taken", got)
	}

	if got := duplicate.Load(); got != senders-1 {
		t.Errorf("senders refused = %d, want %d", got, senders-1)
	}
}
