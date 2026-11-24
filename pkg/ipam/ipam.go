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

// Package ipam implements the infrastructure provider side of the Cluster API IPAM contract
// for KubevirtMachines: it claims addresses from IPAM pools and turns the allocated
// IPAddresses into a static guest network configuration.
package ipam

import (
	gocontext "context"
	"fmt"
	"net"

	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
)

// ClaimProtectionFinalizer keeps an IPAddressClaim, and with it the allocated address, until the
// KubevirtMachine controller has made sure the virtual machine using the address is gone.
const ClaimProtectionFinalizer = "kubevirtmachine.infrastructure.cluster.x-k8s.io/ip-claim-protection"

// Address is one IPv4 address allocated to an interface.
type Address struct {
	// IP is the allocated address.
	IP net.IP
	// Prefix is the prefix length of the address's subnet.
	Prefix int
	// Gateway is the gateway of the address's subnet, if the pool has one.
	Gateway net.IP
}

// CIDR returns the address in CIDR notation.
func (a Address) CIDR() string {
	return fmt.Sprintf("%s/%d", a.IP, a.Prefix)
}

// InterfaceConfig is the static configuration of one virtual machine interface.
type InterfaceConfig struct {
	// Name is the name of the interface in the virtual machine template.
	Name string
	// MACAddress is the MAC address the interface gets in the virtual machine; it is used to
	// match the interface in the guest.
	MACAddress string
	// Addresses are the addresses allocated to the interface, in pool order.
	Addresses []Address
	// Nameservers are the DNS servers configured in the guest.
	Nameservers []string
	// MTU is the MTU configured in the guest, if set.
	MTU *int32
}

// ClaimName returns the name of the IPAddressClaim of a machine for the index-th pool of an
// interface, as suggested by the Cluster API IPAM proposal.
func ClaimName(machineName, interfaceName string, index int) string {
	return fmt.Sprintf("%s-%s-%d", machineName, interfaceName, index)
}

// MACAddressFromIP derives a locally administered unicast MAC address from an IPv4 address:
// 02:00 followed by the four octets of the address. Addresses that are unique on a network give
// MAC addresses that are unique on it too, and the MAC address of a machine stays the same for
// as long as its address does.
func MACAddressFromIP(ip net.IP) (string, error) {
	ip4 := ip.To4()
	if ip4 == nil {
		return "", fmt.Errorf("%s is not an IPv4 address", ip)
	}
	return fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", ip4[0], ip4[1], ip4[2], ip4[3]), nil
}

// Result is the outcome of ReconcileClaims.
type Result struct {
	// Interfaces holds the configuration of every interface in spec.network, once all
	// addresses are allocated.
	Interfaces []InterfaceConfig
	// Pending names the claims that are not bound to an address yet, with the reason the
	// IPAM provider reported, if any.
	Pending []string
}

// InvalidAddressError reports an allocated IPAddress, or a spec.network entry, that cannot be used.
type InvalidAddressError struct {
	msg string
}

func (e *InvalidAddressError) Error() string { return e.msg }

func invalidf(format string, args ...interface{}) error {
	return &InvalidAddressError{msg: fmt.Sprintf(format, args...)}
}

// IsInvalidAddress returns true if err reports an unusable address or network spec.
func IsInvalidAddress(err error) bool {
	var e *InvalidAddressError
	return errors.As(err, &e)
}

// ReconcileClaims makes sure an IPAddressClaim exists for every pool of every interface in
// spec.network of the KubevirtMachine, and reads the addresses the IPAM provider allocated.
// Claims are created in the namespace of the KubevirtMachine, are controlled by it and carry
// the ClaimProtectionFinalizer. When every claim is bound, Result.Interfaces holds the guest
// configuration; otherwise Result.Pending lists the claims still waiting for an address.
func ReconcileClaims(ctx gocontext.Context, c client.Client, kubevirtMachine *infrav1.KubevirtMachine, clusterName string) (*Result, error) {
	network := kubevirtMachine.Spec.Network
	if network == nil {
		return &Result{}, nil
	}

	templateInterfaces := map[string]kubevirtv1.Interface{}
	if tmpl := kubevirtMachine.Spec.VirtualMachineTemplate.Spec.Template; tmpl != nil {
		for _, iface := range tmpl.Spec.Domain.Devices.Interfaces {
			templateInterfaces[iface.Name] = iface
		}
	}

	result := &Result{}
	for _, ifaceSpec := range network.Interfaces {
		templateIface, ok := templateInterfaces[ifaceSpec.Name]
		if !ok {
			return nil, invalidf("spec.network.interfaces: interface %q is not defined in the virtual machine template", ifaceSpec.Name)
		}

		cfg := InterfaceConfig{
			Name:        ifaceSpec.Name,
			MACAddress:  templateIface.MacAddress,
			Nameservers: ifaceSpec.Nameservers,
			MTU:         ifaceSpec.MTU,
		}

		bound := true
		for i, poolRef := range ifaceSpec.AddressesFromPools {
			pool := ipamv1.IPPoolReference{APIGroup: poolRef.APIGroup, Kind: poolRef.Kind, Name: poolRef.Name}
			claim, err := ensureClaim(ctx, c, kubevirtMachine, clusterName, ClaimName(kubevirtMachine.Name, ifaceSpec.Name, i), pool)
			if err != nil {
				return nil, err
			}

			if claim.Status.AddressRef.Name == "" {
				bound = false
				result.Pending = append(result.Pending, pendingReason(claim))
				continue
			}

			addr, err := readAddress(ctx, c, claim)
			if err != nil {
				return nil, err
			}
			cfg.Addresses = append(cfg.Addresses, addr)
		}

		if !bound {
			continue
		}

		if cfg.MACAddress == "" {
			mac, err := MACAddressFromIP(cfg.Addresses[0].IP)
			if err != nil {
				return nil, invalidf("interface %q: %v", ifaceSpec.Name, err)
			}
			cfg.MACAddress = mac
		}
		result.Interfaces = append(result.Interfaces, cfg)
	}

	if len(result.Pending) > 0 {
		result.Interfaces = nil
	}
	return result, nil
}

func ensureClaim(ctx gocontext.Context, c client.Client, kubevirtMachine *infrav1.KubevirtMachine, clusterName, name string, poolRef ipamv1.IPPoolReference) (*ipamv1.IPAddressClaim, error) {
	claim := &ipamv1.IPAddressClaim{}
	key := client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: name}
	err := c.Get(ctx, key, claim)
	switch {
	case apierrors.IsNotFound(err):
		claim = &ipamv1.IPAddressClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:       name,
				Namespace:  kubevirtMachine.Namespace,
				Labels:     map[string]string{clusterv1.ClusterNameLabel: clusterName},
				Finalizers: []string{ClaimProtectionFinalizer},
			},
			Spec: ipamv1.IPAddressClaimSpec{
				ClusterName: clusterName,
				PoolRef:     poolRef,
			},
		}
		if err := controllerutil.SetControllerReference(kubevirtMachine, claim, c.Scheme()); err != nil {
			return nil, errors.Wrapf(err, "failed to set owner of IPAddressClaim %s", key)
		}
		if err := c.Create(ctx, claim); err != nil {
			return nil, errors.Wrapf(err, "failed to create IPAddressClaim %s", key)
		}
		return claim, nil
	case err != nil:
		return nil, errors.Wrapf(err, "failed to get IPAddressClaim %s", key)
	}

	if !metav1.IsControlledBy(claim, kubevirtMachine) {
		return nil, invalidf("IPAddressClaim %s exists but is not controlled by KubevirtMachine %s", key, kubevirtMachine.Name)
	}
	if claim.Spec.PoolRef != poolRef {
		// spec.network is immutable; this is a KubevirtMachine changed before that was enforced.
		return nil, invalidf("IPAddressClaim %s claims from %s %s/%s, but spec.network references %s %s/%s; spec.network cannot be changed, replace the machine instead",
			key, claim.Spec.PoolRef.Kind, claim.Spec.PoolRef.APIGroup, claim.Spec.PoolRef.Name, poolRef.Kind, poolRef.APIGroup, poolRef.Name)
	}
	if !controllerutil.ContainsFinalizer(claim, ClaimProtectionFinalizer) {
		patch := client.MergeFrom(claim.DeepCopy())
		controllerutil.AddFinalizer(claim, ClaimProtectionFinalizer)
		if err := c.Patch(ctx, claim, patch); err != nil {
			return nil, errors.Wrapf(err, "failed to add finalizer to IPAddressClaim %s", key)
		}
	}
	return claim, nil
}

func pendingReason(claim *ipamv1.IPAddressClaim) string {
	for _, cond := range claim.Status.Conditions {
		if cond.Type == ipamv1.IPAddressClaimReadyCondition && cond.Status == metav1.ConditionFalse && cond.Reason != "" {
			if cond.Message != "" {
				return fmt.Sprintf("%s (%s: %s)", claim.Name, cond.Reason, cond.Message)
			}
			return fmt.Sprintf("%s (%s)", claim.Name, cond.Reason)
		}
	}
	return claim.Name
}

func readAddress(ctx gocontext.Context, c client.Client, claim *ipamv1.IPAddressClaim) (Address, error) {
	ipAddress := &ipamv1.IPAddress{}
	key := client.ObjectKey{Namespace: claim.Namespace, Name: claim.Status.AddressRef.Name}
	if err := c.Get(ctx, key, ipAddress); err != nil {
		return Address{}, errors.Wrapf(err, "failed to get IPAddress %s of IPAddressClaim %s", key, claim.Name)
	}

	ip := net.ParseIP(ipAddress.Spec.Address)
	if ip == nil || ip.To4() == nil {
		return Address{}, invalidf("IPAddress %s: %q is not an IPv4 address", key, ipAddress.Spec.Address)
	}
	if ipAddress.Spec.Prefix == nil || *ipAddress.Spec.Prefix < 1 || *ipAddress.Spec.Prefix > 32 {
		return Address{}, invalidf("IPAddress %s: missing or invalid IPv4 prefix", key)
	}

	addr := Address{IP: ip.To4(), Prefix: int(*ipAddress.Spec.Prefix)}
	if ipAddress.Spec.Gateway != "" {
		gw := net.ParseIP(ipAddress.Spec.Gateway)
		if gw == nil || gw.To4() == nil {
			return Address{}, invalidf("IPAddress %s: gateway %q is not an IPv4 address", key, ipAddress.Spec.Gateway)
		}
		addr.Gateway = gw.To4()
	}
	return addr, nil
}

// OwnedClaims returns the IPAddressClaims controlled by the KubevirtMachine. It goes by ownership,
// not by spec.network, so that it also finds the claims of entries removed from spec.network
// before the field was made immutable.
func OwnedClaims(ctx gocontext.Context, c client.Client, kubevirtMachine *infrav1.KubevirtMachine) ([]ipamv1.IPAddressClaim, error) {
	claims := &ipamv1.IPAddressClaimList{}
	if err := c.List(ctx, claims, client.InNamespace(kubevirtMachine.Namespace)); err != nil {
		return nil, errors.Wrapf(err, "failed to list IPAddressClaims in namespace %s", kubevirtMachine.Namespace)
	}

	var owned []ipamv1.IPAddressClaim
	for i := range claims.Items {
		if metav1.IsControlledBy(&claims.Items[i], kubevirtMachine) {
			owned = append(owned, claims.Items[i])
		}
	}
	return owned, nil
}

// ReleaseClaims removes the ClaimProtectionFinalizer from, and deletes, the IPAddressClaims, which
// returns the addresses to their pools. Call it only once the virtual machine using the addresses
// is gone.
func ReleaseClaims(ctx gocontext.Context, c client.Client, claims []ipamv1.IPAddressClaim) error {
	for i := range claims {
		claim := &claims[i]
		key := client.ObjectKeyFromObject(claim)
		if controllerutil.ContainsFinalizer(claim, ClaimProtectionFinalizer) {
			patch := client.MergeFrom(claim.DeepCopy())
			controllerutil.RemoveFinalizer(claim, ClaimProtectionFinalizer)
			if err := c.Patch(ctx, claim, patch); err != nil && !apierrors.IsNotFound(err) {
				return errors.Wrapf(err, "failed to remove finalizer from IPAddressClaim %s", key)
			}
		}
		if claim.DeletionTimestamp.IsZero() {
			if err := c.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
				return errors.Wrapf(err, "failed to delete IPAddressClaim %s", key)
			}
		}
	}
	return nil
}
