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

package ipam

import (
	"net"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
)

func TestRenderNoCloudNetworkConfig(t *testing.T) {
	g := NewWithT(t)

	data, err := RenderNoCloudNetworkConfig([]InterfaceConfig{
		{
			Name:       "lan",
			MACAddress: "02:00:c0:00:02:0a",
			Addresses: []Address{{
				IP:      net.ParseIP("192.0.2.10").To4(),
				Prefix:  24,
				Gateway: net.ParseIP("192.0.2.1").To4(),
			}},
			Nameservers: []string{"192.0.2.53"},
			MTU:         ptr.To[int32](1500),
		},
		{
			Name:       "storage",
			MACAddress: "02:00:c6:33:64:0a",
			Addresses: []Address{{
				IP:      net.ParseIP("198.51.100.10").To4(),
				Prefix:  25,
				Gateway: net.ParseIP("198.51.100.1").To4(),
			}},
		},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(data)).To(MatchYAML(`
version: 2
ethernets:
  lan:
    match: {macaddress: "02:00:c0:00:02:0a"}
    dhcp4: false
    addresses: [192.0.2.10/24]
    routes: [{to: default, via: 192.0.2.1}]
    nameservers: {addresses: [192.0.2.53]}
    mtu: 1500
  storage:
    match: {macaddress: "02:00:c6:33:64:0a"}
    dhcp4: false
    addresses: [198.51.100.10/25]
`))
}

func TestRenderNetworkDataFollowsDataSource(t *testing.T) {
	g := NewWithT(t)
	interfaces := []InterfaceConfig{{
		Name:       "lan",
		MACAddress: "02:00:c0:00:02:0a",
		Addresses:  []Address{{IP: net.ParseIP("192.0.2.10").To4(), Prefix: 24}},
	}}

	configDrive, err := RenderNetworkData(infrav1.CloudInitDataSourceConfigDrive, interfaces)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(configDrive)).To(ContainSubstring(`"ethernet_mac_address":"02:00:c0:00:02:0a"`))

	noCloud, err := RenderNetworkData(infrav1.CloudInitDataSourceNoCloud, interfaces)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(noCloud)).To(ContainSubstring("version: 2"))

	_, err = RenderNoCloudNetworkConfig([]InterfaceConfig{{Name: "lan"}})
	g.Expect(err).To(HaveOccurred())
}

func TestRenderConfigDriveNetworkDataSingleInterface(t *testing.T) {
	g := NewWithT(t)

	data, err := RenderConfigDriveNetworkData([]InterfaceConfig{{
		Name:       "lan",
		MACAddress: "02:00:c0:00:02:0a",
		Addresses: []Address{{
			IP:      net.ParseIP("192.0.2.10").To4(),
			Prefix:  24,
			Gateway: net.ParseIP("192.0.2.1").To4(),
		}},
		Nameservers: []string{"192.0.2.53", "198.51.100.53"},
		MTU:         ptr.To[int32](1500),
	}})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(data)).To(MatchJSON(`{
		"links": [{"id": "lan", "type": "phy", "ethernet_mac_address": "02:00:c0:00:02:0a", "mtu": 1500}],
		"networks": [{
			"id": "lan-0", "type": "ipv4", "link": "lan",
			"ip_address": "192.0.2.10", "netmask": "255.255.255.0",
			"routes": [{"network": "0.0.0.0", "netmask": "0.0.0.0", "gateway": "192.0.2.1"}]
		}],
		"services": [
			{"type": "dns", "address": "192.0.2.53"},
			{"type": "dns", "address": "198.51.100.53"}
		]
	}`))
}

func TestRenderConfigDriveNetworkDataSingleDefaultRoute(t *testing.T) {
	g := NewWithT(t)

	data, err := RenderConfigDriveNetworkData([]InterfaceConfig{
		{
			Name:       "lan",
			MACAddress: "02:00:c0:00:02:0a",
			Addresses: []Address{
				{IP: net.ParseIP("192.0.2.10").To4(), Prefix: 24},
				{IP: net.ParseIP("198.51.100.10").To4(), Prefix: 25, Gateway: net.ParseIP("198.51.100.1").To4()},
			},
			Nameservers: []string{"192.0.2.53"},
		},
		{
			Name:        "storage",
			MACAddress:  "02:00:cb:00:71:0a",
			Addresses:   []Address{{IP: net.ParseIP("203.0.113.10").To4(), Prefix: 26, Gateway: net.ParseIP("203.0.113.1").To4()}},
			Nameservers: []string{"192.0.2.53"},
		},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(data)).To(MatchJSON(`{
		"links": [
			{"id": "lan", "type": "phy", "ethernet_mac_address": "02:00:c0:00:02:0a"},
			{"id": "storage", "type": "phy", "ethernet_mac_address": "02:00:cb:00:71:0a"}
		],
		"networks": [
			{"id": "lan-0", "type": "ipv4", "link": "lan", "ip_address": "192.0.2.10", "netmask": "255.255.255.0"},
			{"id": "lan-1", "type": "ipv4", "link": "lan", "ip_address": "198.51.100.10", "netmask": "255.255.255.128",
			 "routes": [{"network": "0.0.0.0", "netmask": "0.0.0.0", "gateway": "198.51.100.1"}]},
			{"id": "storage-0", "type": "ipv4", "link": "storage", "ip_address": "203.0.113.10", "netmask": "255.255.255.192"}
		],
		"services": [{"type": "dns", "address": "192.0.2.53"}]
	}`))
}

func TestRenderConfigDriveNetworkDataRequiresMAC(t *testing.T) {
	g := NewWithT(t)

	_, err := RenderConfigDriveNetworkData([]InterfaceConfig{{
		Name:      "lan",
		Addresses: []Address{{IP: net.ParseIP("192.0.2.10").To4(), Prefix: 24}},
	}})
	g.Expect(err).To(HaveOccurred())
}
