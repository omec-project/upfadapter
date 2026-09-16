// SPDX-FileCopyrightText: 2026 Forsway Scandinavia AB
//
// SPDX-License-Identifier: Apache-2.0

package udp

import "testing"

// Answering a user plane leaves a transaction table keyed by its own address, and nothing in the
// transaction machinery removes an empty one -- so without this, every peer ever answered costs an
// entry for the life of the process. The release happens when the SMF stops naming that user
// plane, at which point its reports are refused anyway.
func TestDeleteByHostReleasesEveryPortSeenForOnePeer(t *testing.T) {
	table := &ConsumerTable{}

	for _, addr := range []string{"10.0.0.1:8805", "10.0.0.1:41234", "10.0.0.2:8805"} {
		table.LoadOrStore(addr, &TxTable{})
	}

	table.DeleteByHost("10.0.0.1")

	for _, gone := range []string{"10.0.0.1:8805", "10.0.0.1:41234"} {
		if _, found := table.Load(gone); found {
			t.Errorf("%s is still held; the peer chooses its source port, so one guessed port is not enough", gone)
		}
	}

	if _, found := table.Load("10.0.0.2:8805"); !found {
		t.Error("another user plane's table was released with it")
	}
}
