/*
 * Copyright (c) 2019, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	"log"
	"os"
	"strings"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	envDisableHealthChecks = "DP_DISABLE_HEALTHCHECKS"
	allHealthChecks        = "xids"
)

type Device struct {
	pluginapi.Device
	Path string
}

type ResourceManager interface {
	Devices() []*Device
	CheckHealth(stop <-chan interface{}, devices []*Device, unhealthy chan<- *Device)
}

type GpuDeviceManager struct {
	skipMigEnabledGPUs bool
}

type MigDeviceManager struct {
	strategy MigStrategy
	resource string
}

func check(err error) {
	if err != nil {
		log.Panicln("Fatal:", err)
	}
}

func NewGpuDeviceManager(skipMigEnabledGPUs bool) *GpuDeviceManager {
	return &GpuDeviceManager{
		skipMigEnabledGPUs: skipMigEnabledGPUs,
	}
}

func NewMigDeviceManager(strategy MigStrategy, resource string) *MigDeviceManager {
	return &MigDeviceManager{
		strategy: strategy,
		resource: resource,
	}
}

func (g *GpuDeviceManager) Devices() []*Device {
	n, err := nvml.GetDeviceCount()
	check(err)

	var devs []*Device
	for i := uint(0); i < n; i++ {
		d, err := nvml.NewDeviceLite(i)
		check(err)

		migEnabled, err := d.IsMigEnabled()
		check(err)

		if migEnabled && g.skipMigEnabledGPUs {
			continue
		}

		devs = append(devs, buildDevice(d))
	}

	return devs
}

func (m *MigDeviceManager) Devices() []*Device {
	n, err := nvml.GetDeviceCount()
	check(err)

	var devs []*Device
	for i := uint(0); i < n; i++ {
		d, err := nvml.NewDeviceLite(i)
		check(err)

		migEnabled, err := d.IsMigEnabled()
		check(err)

		if !migEnabled {
			continue
		}

		migs, err := d.GetMigDevices()
		check(err)

		for _, mig := range migs {
			if !m.strategy.MatchesResource(mig, m.resource) {
				continue
			}
			devs = append(devs, buildDevices(mig, *vgpuMem)...)
		}
	}

	return devs
}

func (g *GpuDeviceManager) CheckHealth(stop <-chan interface{}, devices []*Device, unhealthy chan<- *Device) {
	checkHealth(stop, devices, unhealthy)
}

func (g *MigDeviceManager) CheckHealth(stop <-chan interface{}, devices []*Device, unhealthy chan<- *Device) {
	checkHealth(stop, devices, unhealthy)
}

func buildDevice(d *nvml.Device) *Device {
	dev := Device{}
	dev.ID = d.UUID
	dev.Health = pluginapi.Healthy
	dev.Path = d.Path
	if d.CPUAffinity != nil {
		dev.Topology = &pluginapi.TopologyInfo{
			Nodes: []*pluginapi.NUMANode{
				&pluginapi.NUMANode{
					ID: int64(*(d.CPUAffinity)),
				},
			},
		}
	}
	return &dev
}
func genVirtualUUID(uuid string, index uint64) string {
	return fmt.Sprintf("%s_%v", uuid, index)
}

func getTrueUUID(uuid string) string {
	return strings.Split(uuid, "_")[0]
}
func buildDevices(d *nvml.Device, vgpuMem int) []*Device {
	num := *d.Memory / 1024 / uint64(vgpuMem)
	devs := []*Device{}
	for i := uint64(0); i < num; i++ {
		dev := Device{}
		dev.ID = genVirtualUUID(d.UUID, i)
		dev.Health = pluginapi.Healthy
		dev.Path = d.Path
		if d.CPUAffinity != nil {
			dev.Topology = &pluginapi.TopologyInfo{
				Nodes: []*pluginapi.NUMANode{
					&pluginapi.NUMANode{
						ID: int64(*(d.CPUAffinity)),
					},
				},
			}
		}
		devs = append(devs, &dev)
	}

	return devs
}

func checkHealth(stop <-chan interface{}, devices []*Device, unhealthy chan<- *Device) {
	disableHealthChecks := strings.ToLower(os.Getenv(envDisableHealthChecks))
	if disableHealthChecks == "all" {
		disableHealthChecks = allHealthChecks
	}
	if strings.Contains(disableHealthChecks, "xids") {
		return
	}

	eventSet := nvml.NewEventSet()
	defer nvml.DeleteEventSet(eventSet)

	for _, d := range devices {
		// true device正常则正常
		trueUUID := getTrueUUID(d.ID)
		gpu, _, _, err := nvml.ParseMigDeviceUUID(trueUUID)
		if err != nil {
			gpu = d.ID
		}

		err = nvml.RegisterEventForDevice(eventSet, nvml.XidCriticalError, gpu)
		if err != nil && strings.HasSuffix(err.Error(), "Not Supported") {
			log.Printf("Warning: %s is too old to support healthchecking: %s. Marking it unhealthy.", d.ID, err)
			unhealthy <- d
			continue
		}
		check(err)
	}

	for {
		select {
		case <-stop:
			return
		default:
		}

		e, err := nvml.WaitForEvent(eventSet, 5000)
		if err != nil && e.Etype != nvml.XidCriticalError {
			continue
		}

		// FIXME: formalize the full list and document it.
		// http://docs.nvidia.com/deploy/xid-errors/index.html#topic_4
		// Application errors: the GPU should still be healthy
		if e.Edata == 31 || e.Edata == 43 || e.Edata == 45 {
			continue
		}

		if e.UUID == nil || len(*e.UUID) == 0 {
			// All devices are unhealthy
			log.Printf("XidCriticalError: Xid=%d, All devices will go unhealthy.", e.Edata)
			for _, d := range devices {
				unhealthy <- d
			}
			continue
		}

		for _, d := range devices {
			// Please see https://github.com/NVIDIA/gpu-monitoring-tools/blob/148415f505c96052cb3b7fdf443b34ac853139ec/bindings/go/nvml/nvml.h#L1424
			// for the rationale why gi and ci can be set as such when the UUID is a full GPU UUID and not a MIG device UUID.
			trueUUID := getTrueUUID(d.ID)
			gpu, gi, ci, err := nvml.ParseMigDeviceUUID(trueUUID)
			if err != nil {
				gpu = d.ID
				gi = 0xFFFFFFFF
				ci = 0xFFFFFFFF
			}

			if gpu == *e.UUID && gi == *e.GpuInstanceId && ci == *e.ComputeInstanceId {
				log.Printf("XidCriticalError: Xid=%d on Device=%s, the device will go unhealthy.", e.Edata, d.ID)
				unhealthy <- d
			}
		}
	}
}
