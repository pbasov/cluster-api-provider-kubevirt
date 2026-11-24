package e2e_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
)

const (
	inClusterIPPoolCRD = "inclusterippools.ipam.cluster.x-k8s.io"
	capkNamespace      = "capk-system"
	capkDeployment     = "capk-controller-manager"
	ipamFeatureGate    = "KubevirtIPAM=true"
)

// skipUnlessIPAMIsAvailable skips the spec unless the Cluster API in-cluster IPAM provider is
// installed and the CAPK manager runs with the KubevirtIPAM feature gate, as with the
// config/components/ipam kustomize component.
func skipUnlessIPAMIsAvailable(ctx context.Context) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := k8sclient.Get(ctx, client.ObjectKey{Name: inClusterIPPoolCRD}, crd); k8serrors.IsNotFound(err) {
		Skip("the Cluster API in-cluster IPAM provider is not installed (no " + inClusterIPPoolCRD + " CRD)")
	} else {
		Expect(err).ToNot(HaveOccurred())
	}

	deployment := &appsv1.Deployment{}
	if err := k8sclient.Get(ctx, client.ObjectKey{Namespace: capkNamespace, Name: capkDeployment}, deployment); k8serrors.IsNotFound(err) {
		Skip("the CAPK manager deployment " + capkNamespace + "/" + capkDeployment + " was not found")
	} else {
		Expect(err).ToNot(HaveOccurred())
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, arg := range container.Args {
			if strings.HasPrefix(arg, "--feature-gates=") && strings.Contains(arg, ipamFeatureGate) {
				return
			}
		}
	}
	Skip("the CAPK manager does not run with --feature-gates=" + ipamFeatureGate)
}

// This spec checks what CAPK does with spec.network, not the guest: the VMs are created halted,
// so it needs neither a network without DHCP nor a guest image. It also covers machines that share
// a bootstrap data secret, as the machines of a MachineDeployment do with some bootstrap providers.
var _ = Describe("Static addresses from an IPAM pool", Label("ipam"), func() {
	const (
		clusterName     = "kvcluster"
		poolName        = "e2e-static-pool"
		bootstrapSecret = "e2e-shared-bootstrap-data"
		machineCount    = 2
	)

	var namespace string

	BeforeEach(func(ctx context.Context) {
		skipUnlessIPAMIsAvailable(ctx)

		namespace = "e2e-test-ipam-" + rand.String(6)
		Expect(k8sclient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
	})

	AfterEach(func(ctx context.Context) {
		if namespace == "" {
			return
		}
		DeleteAndWait(ctx, k8sclient, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: clusterName}}, 300)
		DeleteAndWait(ctx, k8sclient, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, 300)
		namespace = ""
	})

	kubevirtMachine := func(name string) *infrav1.KubevirtMachine {
		return &infrav1.KubevirtMachine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: infrav1.KubevirtMachineSpec{
				BootstrapCheckSpec: infrav1.VirtualMachineBootstrapCheckSpec{CheckStrategy: "none"},
				VirtualMachineTemplate: infrav1.VirtualMachineTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Namespace: namespace},
					Spec: kubevirtv1.VirtualMachineSpec{
						RunStrategy: ptr.To(kubevirtv1.RunStrategyHalted),
						Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
							Spec: kubevirtv1.VirtualMachineInstanceSpec{
								Domain: kubevirtv1.DomainSpec{
									Memory: &kubevirtv1.Memory{Guest: ptr.To(resource.MustParse("128Mi"))},
									Devices: kubevirtv1.Devices{Interfaces: []kubevirtv1.Interface{{
										Name:                   "lan",
										InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{Bridge: &kubevirtv1.InterfaceBridge{}},
									}}},
								},
								Networks: []kubevirtv1.Network{{Name: "lan", NetworkSource: kubevirtv1.NetworkSource{Pod: &kubevirtv1.PodNetwork{}}}},
							},
						},
					},
				},
				Network: &infrav1.NetworkSpec{Interfaces: []infrav1.NetworkInterfaceSpec{{
					Name:               "lan",
					AddressesFromPools: []infrav1.IPPoolReference{{APIGroup: "ipam.cluster.x-k8s.io", Kind: "InClusterIPPool", Name: poolName}},
					Nameservers:        []string{"192.0.2.53"},
				}}},
			},
		}
	}

	It("claims distinct addresses, writes per-machine network data and releases the addresses on delete", func(ctx context.Context) {
		By("creating an InClusterIPPool")
		pool := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "ipam.cluster.x-k8s.io/v1alpha2",
			"kind":       "InClusterIPPool",
			"metadata":   map[string]interface{}{"name": poolName, "namespace": namespace},
			"spec": map[string]interface{}{
				"addresses": []interface{}{"192.0.2.10-192.0.2.19"},
				"prefix":    int64(24),
				"gateway":   "192.0.2.1",
			},
		}}
		Expect(k8sclient.Create(ctx, pool)).To(Succeed())

		By("creating a cluster with machines that share one bootstrap data secret")
		kubevirtCluster := &infrav1.KubevirtCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace}}
		Expect(k8sclient.Create(ctx, kubevirtCluster)).To(Succeed())
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: clusterv1.ClusterSpec{InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrav1.GroupVersion.Group, Kind: "KubevirtCluster", Name: clusterName,
			}},
		}
		Expect(k8sclient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sclient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: bootstrapSecret, Namespace: namespace, Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName}},
			Data:       map[string][]byte{"value": []byte("#cloud-config\n")},
		})).To(Succeed())

		var kubevirtMachineNames []string
		for i := range machineCount {
			name := fmt.Sprintf("%s-static-%d", clusterName, i)
			kubevirtMachineNames = append(kubevirtMachineNames, name)
			Expect(k8sclient.Create(ctx, kubevirtMachine(name))).To(Succeed())
			Expect(k8sclient.Create(ctx, &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName}},
				Spec: clusterv1.MachineSpec{
					ClusterName:       clusterName,
					Bootstrap:         clusterv1.Bootstrap{DataSecretName: ptr.To(bootstrapSecret)},
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: infrav1.GroupVersion.Group, Kind: "KubevirtMachine", Name: name},
				},
			})).To(Succeed())
		}

		By("waiting for every machine to get an address and a VM")
		addresses := map[string]string{}
		Eventually(func(g Gomega) {
			for _, name := range kubevirtMachineNames {
				kvm := &infrav1.KubevirtMachine{}
				g.Expect(k8sclient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, kvm)).To(Succeed())
				g.Expect(conditions.IsTrue(kvm, infrav1.IPAddressClaimedCondition)).To(BeTrue(), "IPAddressClaimed of %s: %v", name, conditions.Get(kvm, infrav1.IPAddressClaimedCondition))

				claim := &ipamv1.IPAddressClaim{}
				g.Expect(k8sclient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name + "-lan-0"}, claim)).To(Succeed())
				g.Expect(metav1.IsControlledBy(claim, kvm)).To(BeTrue())
				ipAddress := &ipamv1.IPAddress{}
				g.Expect(k8sclient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: claim.Status.AddressRef.Name}, ipAddress)).To(Succeed())
				addresses[name] = ipAddress.Spec.Address

				vm := &kubevirtv1.VirtualMachine{}
				g.Expect(k8sclient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, vm)).To(Succeed())
			}
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		By("checking the addresses are distinct and every machine has its own network data")
		seen := map[string]bool{}
		for _, name := range kubevirtMachineNames {
			address := addresses[name]
			Expect(seen).ToNot(HaveKey(address), "address %s allocated twice", address)
			seen[address] = true

			var octets [4]int
			_, err := fmt.Sscanf(address, "%d.%d.%d.%d", &octets[0], &octets[1], &octets[2], &octets[3])
			Expect(err).ToNot(HaveOccurred(), "%s is not an IPv4 address", address)
			mac := fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", octets[0], octets[1], octets[2], octets[3])

			vm := &kubevirtv1.VirtualMachine{}
			Expect(k8sclient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, vm)).To(Succeed())
			Expect(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress).To(Equal(mac))
			var drive *kubevirtv1.CloudInitConfigDriveSource
			for _, volume := range vm.Spec.Template.Spec.Volumes {
				if volume.CloudInitConfigDrive != nil {
					drive = volume.CloudInitConfigDrive
				}
			}
			Expect(drive).ToNot(BeNil())
			Expect(drive.UserDataSecretRef.Name).To(Equal(bootstrapSecret + "-userdata"))
			Expect(drive.NetworkDataSecretRef.Name).To(Equal(name + "-networkdata"))

			networkDataSecret := &corev1.Secret{}
			Expect(k8sclient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name + "-networkdata"}, networkDataSecret)).To(Succeed())
			Expect(string(networkDataSecret.Data["networkdata"])).To(ContainSubstring(`"ip_address":"` + address + `"`))
			Expect(string(networkDataSecret.Data["networkdata"])).To(ContainSubstring(`"ethernet_mac_address":"` + mac + `"`))
		}
		userDataSecret := &corev1.Secret{}
		Expect(k8sclient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: bootstrapSecret + "-userdata"}, userDataSecret)).To(Succeed())
		Expect(userDataSecret.Data).ToNot(HaveKey("networkdata"))

		By("deleting the machines and checking their addresses and network data are released")
		for _, name := range kubevirtMachineNames {
			DeleteAndWait(ctx, k8sclient, &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}, 300)
		}
		Eventually(func(g Gomega) {
			for _, name := range kubevirtMachineNames {
				for _, obj := range []client.Object{
					&infrav1.KubevirtMachine{},
					&kubevirtv1.VirtualMachine{},
					&ipamv1.IPAddressClaim{ObjectMeta: metav1.ObjectMeta{Name: name + "-lan-0"}},
					&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-networkdata"}},
				} {
					key := client.ObjectKey{Namespace: namespace, Name: name}
					if obj.GetName() != "" {
						key.Name = obj.GetName()
					}
					err := k8sclient.Get(ctx, key, obj)
					g.Expect(k8serrors.IsNotFound(err)).To(BeTrue(), "%T %s still exists (err %v)", obj, key, err)
				}
			}
			ipAddresses := &ipamv1.IPAddressList{}
			g.Expect(k8sclient.List(ctx, ipAddresses, client.InNamespace(namespace))).To(Succeed())
			g.Expect(ipAddresses.Items).To(BeEmpty(), "the addresses are returned to the pool")
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	})
})
