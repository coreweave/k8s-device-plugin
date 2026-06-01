/**
# Copyright 2024 NVIDIA CORPORATION
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package plugin

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	spec "github.com/NVIDIA/k8s-device-plugin/api/config/v1"
	"github.com/NVIDIA/k8s-device-plugin/cmd/mps-control-daemon/mps"
	"github.com/NVIDIA/k8s-device-plugin/internal/rm"
)

type mpsOptions struct {
	enabled      bool
	resourceName spec.ResourceName
	daemon       *mps.Daemon
	hostRoot     mps.Root
}

// getMPSOptions returns the MPS options specified for the resource manager.
// If MPS is not configured and empty set of options is returned.
func (o *options) getMPSOptions(resourceManager rm.ResourceManager) (mpsOptions, error) {
	if o.config.Sharing.SharingStrategy() != spec.SharingStrategyMPS {
		return mpsOptions{}, nil
	}

	// TODO: It might make sense to pull this logic into a resource manager.
	for _, device := range resourceManager.Devices() {
		if device.IsMigDevice() {
			return mpsOptions{}, errors.New("sharing using MPS is not supported for MIG devices")
		}
	}

	m := mpsOptions{
		enabled:      true,
		resourceName: resourceManager.Resource(),
		daemon:       mps.NewDaemon(resourceManager, mps.ContainerRoot),
		hostRoot:     mps.Root(*o.config.Flags.CommandLineFlags.MpsRoot),
	}
	return m, nil
}

func (m *mpsOptions) waitForDaemon() error {
	if m == nil || !m.enabled {
		return nil
	}
	// TODO: Check the .ready file here.
	// TODO: Have some retry strategy here.
	if err := m.daemon.AssertHealthy(); err != nil {
		return fmt.Errorf("error checking MPS daemon health: %w", err)
	}
	klog.InfoS("MPS daemon is healthy", "resource", m.resourceName)
	return nil
}

func (m *mpsOptions) updateReponse(response *pluginapi.ContainerAllocateResponse, grantedCount int) {
	if m == nil || !m.enabled {
		return
	}
	// TODO: We should check that the deviceIDs are shared using MPS.
	response.Envs["CUDA_MPS_PIPE_DIRECTORY"] = m.daemon.PipeDir()

	response.Mounts = append(response.Mounts,
		&pluginapi.Mount{
			ContainerPath: m.daemon.PipeDir(),
			HostPath:      m.hostRoot.PipeDir(m.resourceName),
		},
		&pluginapi.Mount{
			ContainerPath: m.daemon.ShmDir(),
			HostPath:      m.hostRoot.ShmDir(m.resourceName),
		},
	)

	// The MPS control daemon is configured with per-device defaults at full
	// hardware. When a container has been granted every replica the plugin
	// advertises on this node, kubelet's accounting guarantees no other pod
	// holds any replica concurrently, so it can use the full daemon defaults —
	// no client-side env vars are needed.
	//
	// For any other (non-full-node) grant, inject the 1/replicas per-device
	// caps via the client env vars. The MPS server clamps the per-client value
	// to no more than the daemon default; since the default is now full, these
	// env vars are the effective cap. This preserves the historical behavior
	// where every non-full grant got 1/replicas memory and thread percentage,
	// regardless of how many replicas were granted.
	devices := m.daemon.Devices()
	total := len(devices)
	if total == 0 || grantedCount >= total {
		return
	}

	replicasByIndex := make(map[string]uint64)
	totalMemoryByIndex := make(map[string]uint64)
	for _, device := range devices {
		replicasByIndex[device.Index]++
		totalMemoryByIndex[device.Index] = device.TotalMemory
	}

	// All physical GPUs managed by this daemon share the same replicas value
	// (it comes from a single config); pick any.
	var replicasPerGPU uint64
	for _, n := range replicasByIndex {
		replicasPerGPU = n
		break
	}
	if replicasPerGPU == 0 {
		return
	}

	response.Envs["CUDA_MPS_ACTIVE_THREAD_PERCENTAGE"] = fmt.Sprintf("%d", 100/replicasPerGPU)

	limits := make([]string, 0, len(totalMemoryByIndex))
	for index, totalMemory := range totalMemoryByIndex {
		if totalMemory == 0 {
			continue
		}
		limits = append(limits, fmt.Sprintf("%s=%dM", index, totalMemory/replicasPerGPU/1024/1024))
	}
	if len(limits) > 0 {
		sort.Strings(limits)
		response.Envs["CUDA_MPS_PINNED_DEVICE_MEM_LIMIT"] = strings.Join(limits, ",")
	}
}
