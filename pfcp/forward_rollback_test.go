// SPDX-FileCopyrightText: 2026 Forsway Scandinavia AB
//
// SPDX-License-Identifier: Apache-2.0

package pfcp

import (
	"testing"

	"github.com/omec-project/upfadapter/config"
	"github.com/omec-project/upfadapter/types"
	pfcp_message "github.com/wmnsk/go-pfcp/message"
)

// A request is registered before it is sent, because the answer can arrive before the send call
// returns. When the send fails there is nothing to wait for that answer, and a registration left
// behind is worse than useless: the next response carrying this sequence number is handed to a
// requester that has already gone, and the requester it belongs to waits for an answer that was
// given to someone else.
//
// The PFCP server is not listening in this test, which is what makes the send fail.
func TestAForwardThatCouldNotBeSentLeavesNoRegistration(t *testing.T) {
	const seq = uint32(4242)

	upNodeID := *types.NewNodeID("10.42.0.31")
	request := pfcp_message.NewSessionModificationRequest(0, 0, 0x1234, seq, 0)

	if _, err := ForwardPfcpMsgToUpf(request, upNodeID); err == nil {
		t.Fatal("the forward reported success with no server to send through")
	}

	if held := config.GetUpfPfcpTxn(seq); held != nil {
		t.Error("the registration outlived the request it belonged to; the next response under this sequence number would be delivered to it")
	}
}
