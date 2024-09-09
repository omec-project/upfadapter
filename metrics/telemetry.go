// SPDX-FileCopyrightText: 2021 Open Networking Foundation <info@opennetworking.org>
//
// SPDX-License-Identifier: Apache-2.0

/*
* Handles statistics for SMF
*
 */

package metrics

import (
	"log"
	"net/http"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// SmfStats captures SMF level stats
type SmfStats struct {
	n4Msg       *prometheus.CounterVec
}

var smfStats *SmfStats

func initSmfStats() *SmfStats {
	return &SmfStats{
		n4Msg: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "n4_messages_total",
			Help: "N4 interface counters",
		}, []string{"smf_id", "msg_type", "direction", "result", "reason"}),
	}
}

func (ps *SmfStats) register() error {
	if err := prometheus.Register(ps.n4Msg); err != nil {
		return err
	}
	return nil
}

func init() {
	smfStats = initSmfStats()

	if err := smfStats.register(); err != nil {
		log.Panicln("SMF Stats register failed")
	}
}

// InitMetrics initialises SMF stats
func InitMetrics() {
	http.Handle("/metrics", promhttp.Handler())
	err := http.ListenAndServe(":9089", nil)
	if err != nil {
		log.Fatalf("failed to start metrics server: %v", err)
	}
}

// IncrementN4MsgStats increments message level stats
func IncrementN4MsgStats(smfID, msgType, direction, result, reason string) {
	smfStats.n4Msg.WithLabelValues(smfID, msgType, direction, result, reason).Inc()
}
