// SPDX-FileCopyrightText: 2022-present Intel Corporation
// SPDX-FileCopyrightText: 2021 Open Networking Foundation <info@opennetworking.org>
// Copyright 2019 free5GC.org
//
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	utilLogger "github.com/omec-project/util/logger"
)

type Config struct {
	Info          *Info              `yaml:"info"`
	Configuration *Configuration     `yaml:"configuration"`
	Logger        *utilLogger.Logger `yaml:"logger"`
}

type Info struct {
	Description string `yaml:"description,omitempty"`
}

type KafkaInfo struct {
	EnableKafka *bool  `yaml:"enableKafka,omitempty"`
	BrokerUri   string `yaml:"brokerUri,omitempty"`
	Topic       string `yaml:"topicName,omitempty"`
	BrokerPort  int    `yaml:"brokerPort,omitempty"`
}

type Configuration struct {
	KafkaInfo KafkaInfo `yaml:"kafkaInfo,omitempty"`
}
