// SPDX-FileCopyrightText: 2021 Open Networking Foundation <info@opennetworking.org>
// SPDX-FileCopyrightText: 2024-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

/*
* Handles statistics for UPF Adapter
*
 */

package metrics

import (
	"net/http"

	"github.com/omec-project/upfadapter/logger"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// AdapterStats captures SMF level stats
type AdapterStats struct {
	n4Msg *prometheus.CounterVec
}

var upfadapterStats *AdapterStats

func initStats() *AdapterStats {
	return &AdapterStats{
		n4Msg: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "n4_messages_total",
			Help: "N4 interface counters",
		}, []string{"smf_id", "msg_type", "direction", "result", "reason"}),
	}
}

func (ps *AdapterStats) register() error {
	if err := prometheus.Register(ps.n4Msg); err != nil {
		return err
	}
	return nil
}

func init() {
	upfadapterStats = initStats()

	if err := upfadapterStats.register(); err != nil {
		logger.KafkaLog.Panicln("UPF-Adapter Stats register failed")
	}
}

// InitMetrics initialises stats
func InitMetrics() {
	http.Handle("/metrics", promhttp.Handler())
	err := http.ListenAndServe(":9089", nil)
	if err != nil {
		logger.KafkaLog.Fatalf("failed to start metrics server: %v", err)
	}
}

// IncrementN4MsgStats increments message level stats
func IncrementN4MsgStats(smfID, msgType, direction, result, reason string) {
	upfadapterStats.n4Msg.WithLabelValues(smfID, msgType, direction, result, reason).Inc()
}
