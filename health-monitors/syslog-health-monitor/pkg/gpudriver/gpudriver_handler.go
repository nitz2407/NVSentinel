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
	"fmt"
	"log/slog"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// NewGPUDriverErrorHandler creates a new GPUDriverErrorHandler instance.
func NewGPUDriverErrorHandler(nodeName, defaultAgentName,
	defaultComponentClass, checkName string) (*GPUDriverErrorHandler, error) {
	return &GPUDriverErrorHandler{
		nodeName:              nodeName,
		defaultAgentName:      defaultAgentName,
		defaultComponentClass: defaultComponentClass,
		checkName:             checkName,
	}, nil
}

// ProcessLine processes a syslog line and returns health events for detected GPU driver errors.
func (h *GPUDriverErrorHandler) ProcessLine(message string) (*pb.HealthEvents, error) {
	event := h.parseGPUDriverError(message)
	if event == nil {
		return nil, nil
	}

	gpuDriverErrorCounterMetric.WithLabelValues(h.nodeName).Inc()

	slog.Info("GPU driver error detected",
		"gpu_id", event.gpuID,
		"error_code", event.errorCode,
		"node", h.nodeName)
	return h.createHealthEventFromError(event), nil
}

// parseGPUDriverError parses a GPU driver error from the log message.
func (h *GPUDriverErrorHandler) parseGPUDriverError(message string) *gpuDriverErrorEvent {
	matches := reGPUDriverErrorPattern.FindStringSubmatch(message)
	if len(matches) < 4 {
		return nil
	}

	gpuID := matches[1]
	errorCode := matches[2]
	errorDetails := matches[3]

	return &gpuDriverErrorEvent{
		gpuID:        gpuID,
		errorCode:    errorCode,
		errorDetails: errorDetails,
		message:      message,
	}
}

func (h *GPUDriverErrorHandler) createHealthEventFromError(event *gpuDriverErrorEvent) *pb.HealthEvents {
	gpuDriverErrorsReportedMetric.WithLabelValues(h.nodeName, event.gpuID).Inc()

	message := fmt.Sprintf("GPU %s: nvidia-modeset driver error detected. "+
		"Error code: %s, Details: %s. "+
		"This indicates the GPU driver is not coming up properly. "+
		"nvidia-driver-daemonset and device-plugin daemonset may be crashing. "+
		"Original message: %s",
		event.gpuID, event.errorCode, event.errorDetails, event.message)

	healthEvent := &pb.HealthEvent{
		Version:            1,
		Agent:              h.defaultAgentName,
		CheckName:          h.checkName,
		ComponentClass:     h.defaultComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		EntitiesImpacted:   []*pb.Entity{{EntityType: "GPU", EntityValue: event.gpuID}},
		Message:            message,
		IsFatal:            true,
		IsHealthy:          false,
		NodeName:           h.nodeName,
		RecommendedAction:  pb.RecommendedAction_RESTART_BM,
		ErrorCode:          []string{"GPU_DRIVER_ERROR"},
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{healthEvent},
	}
}
