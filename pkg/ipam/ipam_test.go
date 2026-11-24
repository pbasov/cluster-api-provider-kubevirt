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
	gocontext "context"
	"net"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
)

const (
	testNamespace = "default"
	testCluster   = "test-cluster"
	testPool      = "test-pool"
)

func testScheme(g *WithT) *runtime.Scheme {
	s := runtime.NewScheme()
	g.Expect(infrav1.AddToScheme(s)).To(Succeed())
	g.Expect(ipamv1.AddToScheme(s)).To(Succeed())
	g.Expect(corev1.AddToScheme(s)).To(Succeed())
	return s
}

func newKubevirtMachine(templateMAC string, network *infrav1.NetworkSpec) *infrav1.KubevirtMachine {
	return &infrav1.KubevirtMachine{
		TypeMeta: metav1.TypeMeta{Kind: "KubevirtMachine", APIVersion: infrav1.GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: testNamespace,
			UID:       "test-machine-uid",
		},
		Spec: infrav1.KubevirtMachineSpec{
			VirtualMachineTemplate: infrav1.VirtualMachineTemplateSpec{
				Spec: kubevirtv1.VirtualMachineSpec{
					Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
						Spec: kubevirtv1.VirtualMachineInstanceSpec{
							Domain: kubevirtv1.DomainSpec{
								Devices: kubevirtv1.Devices{
									Interfaces: []kubevirtv1.Interface{{Name: "lan", MacAddress: templateMAC}},
								},
							},
						},
					},
				},
			},
			Network: network,
		},
	}
}

func poolRef() infrav1.IPPoolReference {
	return infrav1.IPPoolReference{
		APIGroup: "ipam.cluster.x-k8s.io",
		Kind:     "InClusterIPPool",
		Name:     testPool,
	}
}

func lanNetwork(pools int) *infrav1.NetworkSpec {
	refs := make([]infrav1.IPPoolReference, pools)
	for i := range refs {
		refs[i] = poolRef()
	}
	return &infrav1.NetworkSpec{
		Interfaces: []infrav1.NetworkInterfaceSpec{{
			Name:               "lan",
			AddressesFromPools: refs,
			Nameservers:        []string{"192.0.2.53"},
		}},
	}
}

// boundClaim returns a claim controlled by kvm whose status points at an IPAddress of the same name.
func boundClaim(kvm *infrav1.KubevirtMachine, name string) *ipamv1.IPAddressClaim {
	return &ipamv1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  testNamespace,
			Finalizers: []string{ClaimProtectionFinalizer},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: infrav1.GroupVersion.String(),
				Kind:       "KubevirtMachine",
				Name:       kvm.Name,
				UID:        kvm.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec:   ipamv1.IPAddressClaimSpec{ClusterName: testCluster, PoolRef: ipamv1.IPPoolReference{APIGroup: "ipam.cluster.x-k8s.io", Kind: "InClusterIPPool", Name: testPool}},
		Status: ipamv1.IPAddressClaimStatus{AddressRef: ipamv1.IPAddressReference{Name: name}},
	}
}

func ipAddress(name, address string, prefix int32, gateway string) *ipamv1.IPAddress {
	return &ipamv1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: ipamv1.IPAddressSpec{
			ClaimRef: ipamv1.IPAddressClaimReference{Name: name},
			PoolRef:  ipamv1.IPPoolReference{APIGroup: "ipam.cluster.x-k8s.io", Kind: "InClusterIPPool", Name: testPool},
			Address:  address,
			Prefix:   ptr.To(prefix),
			Gateway:  gateway,
		},
	}
}

func TestClaimName(t *testing.T) {
	g := NewWithT(t)
	g.Expect(ClaimName("cp-abcde", "lan", 0)).To(Equal("cp-abcde-lan-0"))
	g.Expect(ClaimName("cp-abcde", "lan", 2)).To(Equal("cp-abcde-lan-2"))
}

func TestMACAddressFromIP(t *testing.T) {
	g := NewWithT(t)

	mac, err := MACAddressFromIP(net.ParseIP("192.0.2.10"))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(mac).To(Equal("02:00:c0:00:02:0a"))

	parsed, err := net.ParseMAC(mac)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(parsed[0]&0x02).To(Equal(byte(0x02)), "locally administered")
	g.Expect(parsed[0]&0x01).To(Equal(byte(0x00)), "unicast")

	_, err = MACAddressFromIP(net.ParseIP("2001:db8::1"))
	g.Expect(err).To(HaveOccurred())
}

func TestReconcileClaimsWithoutNetwork(t *testing.T) {
	g := NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(testScheme(g)).Build()

	result, err := ReconcileClaims(gocontext.Background(), c, newKubevirtMachine("", nil), testCluster)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(result.Interfaces).To(BeEmpty())
	g.Expect(result.Pending).To(BeEmpty())

	claims := &ipamv1.IPAddressClaimList{}
	g.Expect(c.List(gocontext.Background(), claims)).To(Succeed())
	g.Expect(claims.Items).To(BeEmpty())
}

func TestReconcileClaimsCreatesClaimsAndWaits(t *testing.T) {
	g := NewWithT(t)
	kvm := newKubevirtMachine("", lanNetwork(2))
	c := fake.NewClientBuilder().WithScheme(testScheme(g)).WithObjects(kvm).Build()

	result, err := ReconcileClaims(gocontext.Background(), c, kvm, testCluster)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(result.Interfaces).To(BeNil())
	g.Expect(result.Pending).To(ConsistOf("test-machine-lan-0", "test-machine-lan-1"))

	for _, name := range []string{"test-machine-lan-0", "test-machine-lan-1"} {
		claim := &ipamv1.IPAddressClaim{}
		g.Expect(c.Get(gocontext.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, claim)).To(Succeed())
		g.Expect(claim.Spec.ClusterName).To(Equal(testCluster))
		g.Expect(claim.Spec.PoolRef).To(Equal(ipamv1.IPPoolReference{APIGroup: "ipam.cluster.x-k8s.io", Kind: "InClusterIPPool", Name: testPool}))
		g.Expect(claim.Labels).To(HaveKeyWithValue(clusterv1.ClusterNameLabel, testCluster))
		g.Expect(claim.Finalizers).To(ContainElement(ClaimProtectionFinalizer))
		g.Expect(metav1.IsControlledBy(claim, kvm)).To(BeTrue())
	}
}

func TestReconcileClaimsReportsPoolExhaustion(t *testing.T) {
	g := NewWithT(t)
	kvm := newKubevirtMachine("", lanNetwork(1))
	claim := boundClaim(kvm, "test-machine-lan-0")
	claim.Status.AddressRef = ipamv1.IPAddressReference{}
	claim.Status.Conditions = []metav1.Condition{{
		Type:    ipamv1.IPAddressClaimReadyCondition,
		Status:  metav1.ConditionFalse,
		Reason:  ipamv1.IPAddressClaimReadyPoolExhaustedReason,
		Message: "pool is full",
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme(g)).WithObjects(kvm, claim).Build()

	result, err := ReconcileClaims(gocontext.Background(), c, kvm, testCluster)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(result.Interfaces).To(BeNil())
	g.Expect(result.Pending).To(ConsistOf("test-machine-lan-0 (PoolExhausted: pool is full)"))
}

func TestReconcileClaimsReadsAddresses(t *testing.T) {
	g := NewWithT(t)
	kvm := newKubevirtMachine("", lanNetwork(2))
	c := fake.NewClientBuilder().WithScheme(testScheme(g)).WithObjects(
		kvm,
		boundClaim(kvm, "test-machine-lan-0"),
		boundClaim(kvm, "test-machine-lan-1"),
		ipAddress("test-machine-lan-0", "192.0.2.10", 24, "192.0.2.1"),
		ipAddress("test-machine-lan-1", "198.51.100.7", 25, ""),
	).Build()

	result, err := ReconcileClaims(gocontext.Background(), c, kvm, testCluster)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(result.Pending).To(BeEmpty())
	g.Expect(result.Interfaces).To(HaveLen(1))

	iface := result.Interfaces[0]
	g.Expect(iface.Name).To(Equal("lan"))
	g.Expect(iface.MACAddress).To(Equal("02:00:c0:00:02:0a"), "derived from the first address")
	g.Expect(iface.Nameservers).To(Equal([]string{"192.0.2.53"}))
	g.Expect(iface.Addresses).To(HaveLen(2))
	g.Expect(iface.Addresses[0].CIDR()).To(Equal("192.0.2.10/24"))
	g.Expect(iface.Addresses[0].Gateway.String()).To(Equal("192.0.2.1"))
	g.Expect(iface.Addresses[1].CIDR()).To(Equal("198.51.100.7/25"))
	g.Expect(iface.Addresses[1].Gateway).To(BeNil())
}

func TestReconcileClaimsKeepsTemplateMAC(t *testing.T) {
	g := NewWithT(t)
	kvm := newKubevirtMachine("02:11:22:33:44:55", lanNetwork(1))
	c := fake.NewClientBuilder().WithScheme(testScheme(g)).WithObjects(
		kvm,
		boundClaim(kvm, "test-machine-lan-0"),
		ipAddress("test-machine-lan-0", "192.0.2.10", 24, "192.0.2.1"),
	).Build()

	result, err := ReconcileClaims(gocontext.Background(), c, kvm, testCluster)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(result.Interfaces).To(HaveLen(1))
	g.Expect(result.Interfaces[0].MACAddress).To(Equal("02:11:22:33:44:55"))
}

func TestReconcileClaimsAddsMissingFinalizer(t *testing.T) {
	g := NewWithT(t)
	kvm := newKubevirtMachine("", lanNetwork(1))
	claim := boundClaim(kvm, "test-machine-lan-0")
	claim.Finalizers = nil
	c := fake.NewClientBuilder().WithScheme(testScheme(g)).WithObjects(
		kvm, claim, ipAddress("test-machine-lan-0", "192.0.2.10", 24, "192.0.2.1"),
	).Build()

	_, err := ReconcileClaims(gocontext.Background(), c, kvm, testCluster)
	g.Expect(err).ToNot(HaveOccurred())

	got := &ipamv1.IPAddressClaim{}
	g.Expect(c.Get(gocontext.Background(), client.ObjectKeyFromObject(claim), got)).To(Succeed())
	g.Expect(got.Finalizers).To(ContainElement(ClaimProtectionFinalizer))
}

func TestReconcileClaimsInvalid(t *testing.T) {
	tests := []struct {
		name    string
		network *infrav1.NetworkSpec
		objects func(kvm *infrav1.KubevirtMachine) []client.Object
	}{
		{
			name: "interface missing from the template",
			network: &infrav1.NetworkSpec{Interfaces: []infrav1.NetworkInterfaceSpec{{
				Name: "other", AddressesFromPools: []infrav1.IPPoolReference{poolRef()},
			}}},
		},
		{
			name:    "IPv6 address",
			network: lanNetwork(1),
			objects: func(kvm *infrav1.KubevirtMachine) []client.Object {
				return []client.Object{boundClaim(kvm, "test-machine-lan-0"), ipAddress("test-machine-lan-0", "2001:db8::10", 64, "")}
			},
		},
		{
			name:    "IPv4-mapped IPv6 address",
			network: lanNetwork(1),
			objects: func(kvm *infrav1.KubevirtMachine) []client.Object {
				return []client.Object{boundClaim(kvm, "test-machine-lan-0"), ipAddress("test-machine-lan-0", "::ffff:192.0.2.10", 24, "")}
			},
		},
		{
			name:    "prefix out of range",
			network: lanNetwork(1),
			objects: func(kvm *infrav1.KubevirtMachine) []client.Object {
				return []client.Object{boundClaim(kvm, "test-machine-lan-0"), ipAddress("test-machine-lan-0", "192.0.2.10", 33, "")}
			},
		},
		{
			name:    "IPv6 gateway",
			network: lanNetwork(1),
			objects: func(kvm *infrav1.KubevirtMachine) []client.Object {
				return []client.Object{boundClaim(kvm, "test-machine-lan-0"), ipAddress("test-machine-lan-0", "192.0.2.10", 24, "2001:db8::1")}
			},
		},
		{
			name:    "claim controlled by someone else",
			network: lanNetwork(1),
			objects: func(kvm *infrav1.KubevirtMachine) []client.Object {
				claim := boundClaim(kvm, "test-machine-lan-0")
				claim.OwnerReferences[0].UID = "other-uid"
				return []client.Object{claim}
			},
		},
		{
			name:    "claim from another pool than spec.network references",
			network: lanNetwork(1),
			objects: func(kvm *infrav1.KubevirtMachine) []client.Object {
				claim := boundClaim(kvm, "test-machine-lan-0")
				claim.Spec.PoolRef.Name = "previous-pool"
				return []client.Object{claim}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			kvm := newKubevirtMachine("", tt.network)
			objects := []client.Object{kvm}
			if tt.objects != nil {
				objects = append(objects, tt.objects(kvm)...)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(g)).WithObjects(objects...).Build()

			_, err := ReconcileClaims(gocontext.Background(), c, kvm, testCluster)
			g.Expect(err).To(HaveOccurred())
			g.Expect(IsInvalidAddress(err)).To(BeTrue(), err.Error())
		})
	}
}

func TestReleaseClaims(t *testing.T) {
	// Claims are released by ownership, so the ones spec.network no longer lists do not leak.
	for name, network := range map[string]*infrav1.NetworkSpec{
		"interface still in spec.network":   lanNetwork(1),
		"interface gone from spec.network":  {Interfaces: []infrav1.NetworkInterfaceSpec{{Name: "other", AddressesFromPools: []infrav1.IPPoolReference{poolRef()}}}},
		"spec.network removed after claims": nil,
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			kvm := newKubevirtMachine("", network)
			foreign := boundClaim(kvm, "other-machine-lan-0")
			foreign.OwnerReferences[0].UID = "other-uid"
			c := fake.NewClientBuilder().WithScheme(testScheme(g)).WithObjects(
				kvm, boundClaim(kvm, "test-machine-lan-0"), boundClaim(kvm, "test-machine-removed-0"), foreign,
			).Build()

			claims, err := OwnedClaims(gocontext.Background(), c, kvm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(claims).To(HaveLen(2))
			g.Expect(ReleaseClaims(gocontext.Background(), c, claims)).To(Succeed())

			for _, released := range []string{"test-machine-lan-0", "test-machine-removed-0"} {
				err := c.Get(gocontext.Background(), client.ObjectKey{Namespace: testNamespace, Name: released}, &ipamv1.IPAddressClaim{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "own claim %s released", released)
			}

			kept := &ipamv1.IPAddressClaim{}
			g.Expect(c.Get(gocontext.Background(), client.ObjectKeyFromObject(foreign), kept)).To(Succeed(), "claim controlled by another object left alone")
			g.Expect(kept.Finalizers).To(ContainElement(ClaimProtectionFinalizer))

			// Releasing again is a no-op.
			claims, err = OwnedClaims(gocontext.Background(), c, kvm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(claims).To(BeEmpty())
			g.Expect(ReleaseClaims(gocontext.Background(), c, claims)).To(Succeed())
		})
	}
}
