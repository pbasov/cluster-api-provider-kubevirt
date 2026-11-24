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
	"encoding/json"
	"fmt"
	"net"

	"sigs.k8s.io/yaml"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
)

// NetworkDataSecretName returns the name of the per-machine Secret, in the namespace of the
// KubevirtMachine, that holds the network configuration rendered from spec.network.
func NetworkDataSecretName(machineName string) string {
	return machineName + "-networkdata"
}

// RenderNetworkData renders the interfaces in the network configuration format expected by the
// cloud-init data source: OpenStack network_data.json for configDrive, cloud-init network
// configuration version 2 for noCloud.
func RenderNetworkData(dataSource infrav1.CloudInitDataSource, interfaces []InterfaceConfig) ([]byte, error) {
	if dataSource == infrav1.CloudInitDataSourceNoCloud {
		return RenderNoCloudNetworkConfig(interfaces)
	}
	return RenderConfigDriveNetworkData(interfaces)
}

// The OpenStack network_data.json format read by cloud-init's ConfigDrive datasource, which is
// what KubeVirt's cloudInitConfigDrive networkData ends up as in the guest. Only the subset CAPK
// renders is modelled.
type configDriveNetworkData struct {
	Links    []configDriveLink    `json:"links"`
	Networks []configDriveNetwork `json:"networks"`
	Services []configDriveService `json:"services,omitempty"`
}

type configDriveLink struct {
	ID                 string `json:"id"`
	Type               string `json:"type"`
	EthernetMACAddress string `json:"ethernet_mac_address"`
	MTU                *int32 `json:"mtu,omitempty"`
}

type configDriveNetwork struct {
	ID        string             `json:"id"`
	Type      string             `json:"type"`
	Link      string             `json:"link"`
	IPAddress string             `json:"ip_address"`
	Netmask   string             `json:"netmask"`
	Routes    []configDriveRoute `json:"routes,omitempty"`
}

type configDriveRoute struct {
	Network string `json:"network"`
	Netmask string `json:"netmask"`
	Gateway string `json:"gateway"`
}

type configDriveService struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

// RenderConfigDriveNetworkData renders the interfaces as an OpenStack network_data.json document
// for the cloud-init ConfigDrive datasource. Every interface is matched in the guest by its MAC
// address and gets its addresses statically, so the guest does not run DHCP on it. The first
// address that has a gateway, in interface and pool order, provides the only default route.
func RenderConfigDriveNetworkData(interfaces []InterfaceConfig) ([]byte, error) {
	data := configDriveNetworkData{
		Links:    []configDriveLink{},
		Networks: []configDriveNetwork{},
	}
	defaultRouteSet := false
	seenNameservers := map[string]bool{}

	for _, iface := range interfaces {
		if iface.MACAddress == "" {
			return nil, fmt.Errorf("interface %q has no MAC address", iface.Name)
		}
		data.Links = append(data.Links, configDriveLink{
			ID:                 iface.Name,
			Type:               "phy",
			EthernetMACAddress: iface.MACAddress,
			MTU:                iface.MTU,
		})

		for i, addr := range iface.Addresses {
			network := configDriveNetwork{
				ID:        fmt.Sprintf("%s-%d", iface.Name, i),
				Type:      "ipv4",
				Link:      iface.Name,
				IPAddress: addr.IP.String(),
				Netmask:   net.IP(net.CIDRMask(addr.Prefix, 32)).String(),
			}
			if addr.Gateway != nil && !defaultRouteSet {
				network.Routes = []configDriveRoute{{
					Network: "0.0.0.0",
					Netmask: "0.0.0.0",
					Gateway: addr.Gateway.String(),
				}}
				defaultRouteSet = true
			}
			data.Networks = append(data.Networks, network)
		}

		for _, ns := range iface.Nameservers {
			if seenNameservers[ns] {
				continue
			}
			seenNameservers[ns] = true
			data.Services = append(data.Services, configDriveService{Type: "dns", Address: ns})
		}
	}

	return json.Marshal(data)
}

// The cloud-init network configuration version 2 read by the NoCloud datasource. Only the subset
// CAPK renders is modelled.
type noCloudNetworkConfig struct {
	Version   int                            `json:"version"`
	Ethernets map[string]noCloudEthernetConf `json:"ethernets"`
}

type noCloudEthernetConf struct {
	Match       noCloudMatch        `json:"match"`
	DHCP4       bool                `json:"dhcp4"`
	Addresses   []string            `json:"addresses"`
	Routes      []noCloudRoute      `json:"routes,omitempty"`
	Nameservers *noCloudNameservers `json:"nameservers,omitempty"`
	MTU         *int32              `json:"mtu,omitempty"`
}

type noCloudMatch struct {
	MACAddress string `json:"macaddress"`
}

type noCloudRoute struct {
	To  string `json:"to"`
	Via string `json:"via"`
}

type noCloudNameservers struct {
	Addresses []string `json:"addresses"`
}

// RenderNoCloudNetworkConfig renders the interfaces as a cloud-init network configuration version 2
// document for the NoCloud datasource, with the same semantics as RenderConfigDriveNetworkData:
// interfaces matched by MAC address, static addresses with DHCP disabled, and a single default route
// through the first address that has a gateway.
func RenderNoCloudNetworkConfig(interfaces []InterfaceConfig) ([]byte, error) {
	cfg := noCloudNetworkConfig{Version: 2, Ethernets: map[string]noCloudEthernetConf{}}
	defaultRouteSet := false

	for _, iface := range interfaces {
		if iface.MACAddress == "" {
			return nil, fmt.Errorf("interface %q has no MAC address", iface.Name)
		}
		eth := noCloudEthernetConf{
			Match:     noCloudMatch{MACAddress: iface.MACAddress},
			Addresses: []string{},
			MTU:       iface.MTU,
		}
		for _, addr := range iface.Addresses {
			eth.Addresses = append(eth.Addresses, addr.CIDR())
			if addr.Gateway != nil && !defaultRouteSet {
				eth.Routes = []noCloudRoute{{To: "default", Via: addr.Gateway.String()}}
				defaultRouteSet = true
			}
		}
		if len(iface.Nameservers) > 0 {
			eth.Nameservers = &noCloudNameservers{Addresses: iface.Nameservers}
		}
		cfg.Ethernets[iface.Name] = eth
	}

	return yaml.Marshal(cfg)
}
