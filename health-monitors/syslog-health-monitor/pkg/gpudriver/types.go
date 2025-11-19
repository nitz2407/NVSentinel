// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gpudriver

import (
	"regexp"
)

var (
	// Example: nvidia-modeset: ERROR: GPU:2: Error while waiting for GPU progress: 0x0000c77d:0 2:0:4048:4040
	reGPUDriverErrorPattern = regexp.MustCompile(
		`nvidia-modeset: ERROR: GPU:(\d+): Error while waiting for GPU progress: (0x[0-9a-fA-F:]+)\s+(\d+:\d+:\d+:\d+)`)
)

// This handler is stateless and reports errors immediately.
type GPUDriverErrorHandler struct {
	nodeName              string
	defaultAgentName      string
	defaultComponentClass string
	checkName             string
}

// gpuDriverErrorEvent represents a parsed GPU driver error event
type gpuDriverErrorEvent struct {
	gpuID        string
	errorCode    string
	errorDetails string
	message      string
}
