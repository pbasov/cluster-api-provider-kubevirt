/*
Copyright 2026 The Kubernetes Authors.

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

package kubevirt

import (
	gocontext "context"
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/context"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/ipam"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/testing"
)

var _ = Describe("Static network from spec.network", func() {
	var machineContext *context.MachineContext

	newContext := func(interfaces ...kubevirtv1.Interface) *context.MachineContext {
		kvm := testing.NewKubevirtMachine(kubevirtMachineName, machineName)
		kvm.Spec.VirtualMachineTemplate.Spec.Template.Spec.Domain.Devices.Interfaces = interfaces
		return &context.MachineContext{
			Context:             gocontext.TODO(),
			Cluster:             cluster,
			KubevirtCluster:     kubevirtCluster,
			Machine:             testing.NewMachine(clusterName, machineName, kvm),
			KubevirtMachine:     kvm,
			BootstrapDataSecret: bootstrapDataSecret,
			Logger:              logger,
		}
	}

	staticLan := []ipam.InterfaceConfig{{
		Name:       "lan",
		MACAddress: "02:00:c0:00:02:0a",
		Addresses:  []ipam.Address{{IP: net.ParseIP("192.0.2.10").To4(), Prefix: 24, Gateway: net.ParseIP("192.0.2.1").To4()}},
	}}

	cloudInitVolume := func(vm *kubevirtv1.VirtualMachine) *kubevirtv1.CloudInitConfigDriveSource {
		for _, v := range vm.Spec.Template.Spec.Volumes {
			if v.Name == "cloudinitvolume" {
				return v.CloudInitConfigDrive
			}
		}
		return nil
	}

	It("leaves the VM unchanged when spec.network is unset", func() {
		machineContext = newContext(kubevirtv1.Interface{Name: "lan"})

		vm := newVirtualMachineFromKubevirtMachine(machineContext, "default")

		drive := cloudInitVolume(vm)
		Expect(drive).NotTo(BeNil())
		Expect(drive.UserDataSecretRef.Name).To(Equal("fakeDataSecretName-userdata"))
		Expect(drive.NetworkDataSecretRef).To(BeNil())
		Expect(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress).To(BeEmpty())
	})

	It("references the network data and pins the MAC address", func() {
		machineContext = newContext(kubevirtv1.Interface{Name: "lan"}, kubevirtv1.Interface{Name: "other"})
		machineContext.KubevirtMachine.Spec.Network = &infrav1.NetworkSpec{Interfaces: []infrav1.NetworkInterfaceSpec{{Name: "lan"}}}
		machineContext.StaticNetwork = staticLan

		vm := newVirtualMachineFromKubevirtMachine(machineContext, "default")

		drive := cloudInitVolume(vm)
		Expect(drive).NotTo(BeNil())
		Expect(drive.UserDataSecretRef.Name).To(Equal("fakeDataSecretName-userdata"))
		Expect(drive.NetworkDataSecretRef).NotTo(BeNil())
		Expect(drive.NetworkDataSecretRef.Name).To(Equal("fakeDataSecretName-userdata"))

		interfaces := vm.Spec.Template.Spec.Domain.Devices.Interfaces
		Expect(interfaces[0].MacAddress).To(Equal("02:00:c0:00:02:0a"))
		Expect(interfaces[1].MacAddress).To(BeEmpty(), "interfaces without static configuration are not touched")

		// The KubevirtMachine template itself is not modified.
		Expect(machineContext.KubevirtMachine.Spec.VirtualMachineTemplate.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress).To(BeEmpty())
	})

	It("keeps a MAC address set in the template", func() {
		machineContext = newContext(kubevirtv1.Interface{Name: "lan", MacAddress: "02:11:22:33:44:55"})
		machineContext.StaticNetwork = staticLan

		vm := newVirtualMachineFromKubevirtMachine(machineContext, "default")

		Expect(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress).To(Equal("02:11:22:33:44:55"))
	})

	It("reports the claimed addresses instead of the VMI addresses", func() {
		machineContext = newContext(kubevirtv1.Interface{Name: "lan"})
		machineContext.StaticNetwork = staticLan
		vmi := testing.NewVirtualMachineInstance(machineContext.KubevirtMachine)
		vmi.Status.Interfaces = []kubevirtv1.VirtualMachineInstanceNetworkInterface{
			{IP: "198.51.100.99", IPs: []string{"198.51.100.99", "203.0.113.99"}},
		}
		vm := testing.NewVirtualMachine(vmi)
		c := fake.NewClientBuilder().WithScheme(testing.SetupScheme()).WithObjects([]client.Object{vmi, vm}...).Build()

		externalMachine, err := defaultTestMachine(machineContext, vmi.Namespace, c, FakeVMCommandExecutor{false}, []byte{})
		Expect(err).NotTo(HaveOccurred())
		Expect(externalMachine.Address()).To(Equal("192.0.2.10"))
		Expect(externalMachine.Addresses()).To(Equal([]string{"192.0.2.10"}))
	})

	It("reports the claimed addresses before any VMI exists", func() {
		machineContext = newContext(kubevirtv1.Interface{Name: "lan"})
		machineContext.StaticNetwork = staticLan
		c := fake.NewClientBuilder().WithScheme(testing.SetupScheme()).Build()

		externalMachine, err := defaultTestMachine(machineContext, "default", c, FakeVMCommandExecutor{false}, []byte{})
		Expect(err).NotTo(HaveOccurred())
		Expect(externalMachine.Address()).To(Equal("192.0.2.10"))
	})
})
