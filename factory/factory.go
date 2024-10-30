// SPDX-FileCopyrightText: 2021 Open Networking Foundation <info@opennetworking.org>
// Copyright 2019 free5GC.org
//
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"gopkg.in/yaml.v2"
	"os"
)

var (
	UpfAdapterConfig Config
)

func InitConfigFactory(f string) error {

	if content, err := os.ReadFile(f); err != nil {
		return err
	} else {
		UpfAdapterConfig = Config{}

		if yamlErr := yaml.Unmarshal(content, &UpfAdapterConfig); yamlErr != nil {
			return yamlErr
		}

		if UpfAdapterConfig.Configuration.KafkaInfo.EnableKafka == nil {
			enableKafka := true
			UpfAdapterConfig.Configuration.KafkaInfo.EnableKafka = &enableKafka
		}

	}

	return nil
}
