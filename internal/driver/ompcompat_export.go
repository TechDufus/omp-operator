//go:build ompcompat

/*
Copyright 2026 TechDufus.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"time"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

// This file exists only for test/ompcompat, which runs the driver's own omp command line and
// config writers against the pinned real omp. It is compiled only under the ompcompat tag.

// OmpArgs is the omp command line Run starts.
func OmpArgs(cfg agentapi.SessionConfig, timeout time.Duration, p Paths) []string {
	return ompArgs(cfg, timeout, p)
}

// WriteOmpConfig writes config.yml, models.yml, mcp.json and the system prompt file as Run does.
func (d *Driver) WriteOmpConfig(cfg agentapi.SessionConfig) error {
	return d.writeOmpConfig(cfg)
}
