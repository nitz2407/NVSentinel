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
	"testing"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGPUDriverError(t *testing.T) {
	testCases := []struct {
		name            string
		message         string
		expectEvent     bool
		expectGPUID     string
		expectErrorCode string
	}{
		{
			name:            "Valid nvidia-modeset error GPU 2",
			message:         "nvidia-modeset: ERROR: GPU:2: Error while waiting for GPU progress: 0x0000c77d:0 2:0:4048:4040",
			expectEvent:     true,
			expectGPUID:     "2",
			expectErrorCode: "0x0000c77d:0",
		},
		{
			name:            "Valid nvidia-modeset error GPU 1",
			message:         "nvidia-modeset: ERROR: GPU:1: Error while waiting for GPU progress: 0x0000c77d:0 2:0:4048:4040",
			expectEvent:     true,
			expectGPUID:     "1",
			expectErrorCode: "0x0000c77d:0",
		},
		{
			name:        "Different nvidia-modeset error (not our pattern)",
			message:     "nvidia-modeset: ERROR: GPU:0: Some other error",
			expectEvent: false,
		},
		{
			name:        "Non-matching message",
			message:     "Some other log message",
			expectEvent: false,
		},
		{
			name:        "Partial match without full pattern",
			message:     "nvidia-modeset: ERROR: GPU:2: Different error message",
			expectEvent: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := NewGPUDriverErrorHandler(
				"test-node",
				"test-agent",
				"GPU",
				"test-check",
			)
			require.NoError(t, err)

			event := h.parseGPUDriverError(tc.message)
			if tc.expectEvent {
				require.NotNil(t, event, "Expected to parse an event")
				assert.Equal(t, tc.expectGPUID, event.gpuID)
				assert.Equal(t, tc.expectErrorCode, event.errorCode)
			} else {
				assert.Nil(t, event, "Expected no event to be parsed")
			}
		})
	}
}

func TestProcessLine(t *testing.T) {
	testCases := []struct {
		name          string
		message       string
		expectEvent   bool
		validateEvent func(t *testing.T, events *pb.HealthEvents)
	}{
		{
			name:        "Valid GPU driver error generates health event",
			message:     "nvidia-modeset: ERROR: GPU:2: Error while waiting for GPU progress: 0x0000c77d:0 2:0:4048:4040",
			expectEvent: true,
			validateEvent: func(t *testing.T, events *pb.HealthEvents) {
				require.NotNil(t, events)
				require.Len(t, events.Events, 1)

				event := events.Events[0]
				// Verify health event structure
				assert.Equal(t, "test-agent", event.Agent)
				assert.Equal(t, "test-check", event.CheckName)
				assert.Equal(t, "GPU", event.ComponentClass)
				assert.True(t, event.IsFatal)
				assert.False(t, event.IsHealthy)
				assert.Equal(t, pb.RecommendedAction_RESTART_BM, event.RecommendedAction)
				assert.Contains(t, event.ErrorCode, "GPU_DRIVER_ERROR")

				// Verify GPU entity
				require.Len(t, event.EntitiesImpacted, 1)
				assert.Equal(t, "GPU", event.EntitiesImpacted[0].EntityType)
				assert.Equal(t, "2", event.EntitiesImpacted[0].EntityValue)

				// Verify error details in message
				assert.Contains(t, event.Message, "0x0000c77d:0")
				assert.Contains(t, event.Message, "2:0:4048:4040")
				assert.Contains(t, event.Message, "GPU 2")
				assert.Contains(t, event.Message, "nvidia-modeset driver error")
			},
		},
		{
			name:        "Different GPU IDs handled correctly",
			message:     "nvidia-modeset: ERROR: GPU:5: Error while waiting for GPU progress: 0x0000abcd:1 5:0:1234:5678",
			expectEvent: true,
			validateEvent: func(t *testing.T, events *pb.HealthEvents) {
				require.NotNil(t, events)
				require.Len(t, events.Events, 1)
				event := events.Events[0]
				assert.Equal(t, "5", event.EntitiesImpacted[0].EntityValue)
				assert.Contains(t, event.Message, "GPU 5")
				assert.Contains(t, event.Message, "0x0000abcd:1")
			},
		},
		{
			name:        "Non-matching message returns nil",
			message:     "Some other log message",
			expectEvent: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := NewGPUDriverErrorHandler(
				"test-node",
				"test-agent",
				"GPU",
				"test-check",
			)
			require.NoError(t, err)

			events, err := handler.ProcessLine(tc.message)
			require.NoError(t, err)

			if tc.expectEvent {
				require.NotNil(t, events, "Expected an event to be generated")
				if tc.validateEvent != nil {
					tc.validateEvent(t, events)
				}
			} else {
				assert.Nil(t, events, "Expected no event to be generated")
			}
		})
	}
}
