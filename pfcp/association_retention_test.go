// SPDX-FileCopyrightText: 2026 Forsway Scandinavia AB
//
// SPDX-License-Identifier: Apache-2.0

package pfcp

import (
	"testing"
	"time"

	"github.com/omec-project/upfadapter/config"
	"github.com/omec-project/upfadapter/types"
	"github.com/wmnsk/go-pfcp/ie"
	pfcp_message "github.com/wmnsk/go-pfcp/message"
)

// An SMF that re-associates while the adapter is associated with the UPF is answered from the
// adapter's stored copy of the UPF's first answer. The adapter's own association is not replaced,
// so the UPF still holds the sessions, and TS 29.244 clause 6.2.6.2.2 has the answer say so with
// PSREI when the request asked for them to be retained, and only then. What the stored copy said
// about PSREI answered an earlier request; the UPF's other flags are its own and are kept.
func TestAReAssociationIsAnsweredWithPSREIWhenItAsksForRetention(t *testing.T) {
	// TS 29.244 clause 8.2.134, octet 5: PSREI is bit 1 and UUPSI bit 2. Spelled out here rather
	// than taken from the code under test, so that a wrong bit there fails.
	const (
		bitPSREI = 0x01
		bitUUPSI = 0x02
	)
	for _, tc := range []struct {
		name      string
		stored    uint8 // the flags in the UPF's stored answer; 0 for none
		retention bool  // the SMF's request carries PFCP Session Retention Information
		want      uint8 // the flags the SMF is answered with; 0 for none
	}{
		{"asks, stored answer without flags", 0, true, bitPSREI},
		{"does not ask, stored answer without flags", 0, false, 0},
		{"does not ask, stored answer with PSREI", bitPSREI, false, 0},
		{"asks, stored answer with UUPSI", bitUUPSI, true, bitPSREI | bitUUPSI},
		{"does not ask, stored answer with PSREI and UUPSI", bitPSREI | bitUUPSI, false, bitUUPSI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upNodeID := *types.NewNodeID("10.42.0.40")
			config.InsertUpfNode(upNodeID)
			upf := config.GetUpfFromNodeId(&upNodeID)
			t.Cleanup(func() {
				config.UpfCfg.UpfListLock.Lock()
				delete(config.UpfCfg.UPFs, string(upNodeID.NodeIdValue))
				config.UpfCfg.UpfListLock.Unlock()
			})
			ies := []*ie.IE{
				ie.NewCause(ie.CauseRequestAccepted), ie.NewNodeID("10.42.0.40", "", ""),
				ie.NewRecoveryTimeStamp(time.Unix(1_790_000_000, 0)),
			}
			if tc.stored != 0 {
				ies = append(ies, ie.NewPFCPASRspFlags(tc.stored))
			}
			upf.LastAssoRsp = *pfcp_message.NewAssociationSetupResponse(1, ies...)
			upf.State = config.AssociatedSetUpSuccess

			reqIEs := []*ie.IE{ie.NewNodeID("10.42.0.1", "", ""), ie.NewRecoveryTimeStamp(time.Now())}
			if tc.retention {
				reqIEs = append(reqIEs, ie.NewPFCPSessionRetentionInformation(nil))
			}
			b, err := ForwardPfcpMsgToUpf(pfcp_message.NewAssociationSetupRequest(7, reqIEs...), upNodeID)
			if err != nil {
				t.Fatalf("answering the re-association: %v", err)
			}
			rsp, err := pfcp_message.ParseAssociationSetupResponse(b)
			if err != nil {
				t.Fatalf("parsing the answer: %v", err)
			}

			var got uint8
			if rsp.PFCPASRspFlags != nil {
				if got, err = rsp.PFCPASRspFlags.PFCPASRspFlags(); err != nil {
					t.Fatalf("reading the answer's flags: %v", err)
				}
			}
			if got != tc.want {
				t.Errorf("answered with flags %#02x, want %#02x", got, tc.want)
			}
			var stored uint8
			if upf.LastAssoRsp.PFCPASRspFlags != nil {
				if stored, err = upf.LastAssoRsp.PFCPASRspFlags.PFCPASRspFlags(); err != nil {
					t.Fatalf("reading the stored answer's flags: %v", err)
				}
			}
			if stored != tc.stored {
				t.Errorf("answering changed the stored copy of the UPF's answer: flags %#02x, were %#02x", stored, tc.stored)
			}
		})
	}
}
