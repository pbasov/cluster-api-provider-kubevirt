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

package controllers

import (
	gocontext "context"
	"encoding/json"
	"time"

	"github.com/go-logr/logr"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/context"
	infraclustermock "sigs.k8s.io/cluster-api-provider-kubevirt/pkg/infracluster/mock"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/ipam"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/kubevirt"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/testing"
	workloadclustermock "sigs.k8s.io/cluster-api-provider-kubevirt/pkg/workloadcluster/mock"
)

var _ = Describe("reconcile a kubevirt machine with spec.network", func() {
	const claimName = "test-kubevirt-machine-lan-0"

	var (
		infraClusterMock *infraclustermock.MockInfraCluster
		machineContext   *context.MachineContext
		reconciler       KubevirtMachineReconciler
		// c is the management cluster client.
		c client.Client
		// infraClient is the infra cluster client; it is c unless the test sets up an external
		// infra cluster.
		infraClient client.Client
		recorder    *events.FakeRecorder
	)

	lanPool := ipamv1.IPPoolReference{APIGroup: "ipam.cluster.x-k8s.io", Kind: "InClusterIPPool", Name: "pool"}

	BeforeEach(func() {
		mockCtrl := gomock.NewController(GinkgoT())
		infraClusterMock = infraclustermock.NewMockInfraCluster(mockCtrl)

		bootstrapSecretName = "bootstrap-secret"
		sshKeySecretName = "ssh-keys"
		clusterName = "kvcluster"
		machineName = "test-machine"
		kubevirtMachineName = "test-kubevirt-machine"

		kubevirtMachine = testing.NewKubevirtMachine(kubevirtMachineName, machineName)
		kubevirtMachine.UID = "test-kubevirt-machine-uid"
		kubevirtMachine.Spec.VirtualMachineTemplate.Spec.Template.Spec.Domain.Devices.Interfaces = []kubevirtv1.Interface{{
			Name:                   "lan",
			InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{Bridge: &kubevirtv1.InterfaceBridge{}},
		}}
		kubevirtMachine.Spec.Network = &infrav1.NetworkSpec{
			Interfaces: []infrav1.NetworkInterfaceSpec{{
				Name:               "lan",
				AddressesFromPools: []infrav1.IPPoolReference{{APIGroup: lanPool.APIGroup, Kind: lanPool.Kind, Name: lanPool.Name}},
				Nameservers:        []string{"192.0.2.53"},
			}},
		}

		kubevirtCluster = testing.NewKubevirtCluster(clusterName, machineName)
		kubevirtCluster.Spec.SshKeys = infrav1.SSHKeys{DataSecretName: &sshKeySecretName}
		cluster = testing.NewCluster(clusterName, kubevirtCluster)
		machine = testing.NewMachine(clusterName, machineName, kubevirtMachine)
		machine.Spec = clusterv1.MachineSpec{Bootstrap: clusterv1.Bootstrap{DataSecretName: &bootstrapSecretName}}

		sshKeySecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sshKeySecretName},
			Data:       map[string][]byte{"pub": []byte("sha-rsa 1234"), "key": []byte("sha-rsa 5678")},
		}
		bootstrapSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: bootstrapSecretName},
			Data:       map[string][]byte{"value": []byte("shell-script")},
		}
	})

	newMachineContext := func(m *clusterv1.Machine, kvm *infrav1.KubevirtMachine) *context.MachineContext {
		return &context.MachineContext{
			Context:         logr.NewContext(gocontext.Background(), GinkgoLogr),
			Cluster:         cluster,
			KubevirtCluster: kubevirtCluster,
			Machine:         m,
			KubevirtMachine: kvm,
			Logger:          ctrl.Log.WithName("test"),
		}
	}

	// setupWithInfraClient builds the management cluster client from objects and uses infra as the
	// infra cluster client, or the management cluster client when infra is nil.
	setupWithInfraClient := func(infra client.Client, objects ...client.Object) {
		objects = append([]client.Object{cluster, kubevirtCluster, machine, kubevirtMachine, sshKeySecret, bootstrapSecret}, objects...)
		c = fake.NewClientBuilder().WithScheme(testing.SetupScheme()).WithObjects(objects...).WithStatusSubresource(objects...).Build()
		infraClient = infra
		if infraClient == nil {
			infraClient = c
		}
		machineContext = newMachineContext(machine, kubevirtMachine)
		recorder = events.NewFakeRecorder(10)
		reconciler = KubevirtMachineReconciler{
			Client:          c,
			InfraCluster:    infraClusterMock,
			WorkloadCluster: workloadclustermock.NewMockWorkloadCluster(gomock.NewController(GinkgoT())),
			MachineFactory:  kubevirt.DefaultMachineFactory{},
			IPAMEnabled:     true,
			Recorder:        recorder,
		}
		infraClusterMock.EXPECT().GenerateInfraClusterClient(gomock.Any(), gomock.Any(), gomock.Any()).Return(infraClient, kubevirtMachine.Namespace, nil).AnyTimes()
	}

	setup := func(objects ...client.Object) {
		setupWithInfraClient(nil, objects...)
	}

	// boundClaimAndAddress returns the IPAddressClaim of the lan interface of kvm, bound to an
	// IPAddress with the given address.
	boundClaimAndAddress := func(kvm *infrav1.KubevirtMachine, address string) []client.Object {
		name := ipam.ClaimName(kvm.Name, "lan", 0)
		claim := &ipamv1.IPAddressClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:       name,
				Finalizers: []string{ipam.ClaimProtectionFinalizer},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: infrav1.GroupVersion.String(),
					Kind:       "KubevirtMachine",
					Name:       kvm.Name,
					UID:        kvm.UID,
					Controller: ptr.To(true),
				}},
			},
			Spec:   ipamv1.IPAddressClaimSpec{ClusterName: clusterName, PoolRef: lanPool},
			Status: ipamv1.IPAddressClaimStatus{AddressRef: ipamv1.IPAddressReference{Name: name}},
		}
		ipAddress := &ipamv1.IPAddress{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: ipamv1.IPAddressSpec{
				ClaimRef: ipamv1.IPAddressClaimReference{Name: name},
				PoolRef:  lanPool,
				Address:  address,
				Prefix:   ptr.To[int32](24),
				Gateway:  "192.0.2.1",
			},
		}
		return []client.Object{claim, ipAddress}
	}

	// peerMachine returns a second Machine and KubevirtMachine, a copy of the first ones, that use
	// the same bootstrap data secret.
	peerMachine := func() (*clusterv1.Machine, *infrav1.KubevirtMachine) {
		kvm := kubevirtMachine.DeepCopy()
		kvm.Name = kubevirtMachineName + "-2"
		kvm.UID = "test-kubevirt-machine-2-uid"
		kvm.OwnerReferences[0].Name = machineName + "-2"
		m := testing.NewMachine(clusterName, machineName+"-2", kvm)
		m.Spec.Bootstrap = clusterv1.Bootstrap{DataSecretName: &bootstrapSecretName}
		return m, kvm
	}

	getVM := func(cl client.Client, kvm *infrav1.KubevirtMachine) *kubevirtv1.VirtualMachine {
		vm := &kubevirtv1.VirtualMachine{}
		ExpectWithOffset(1, cl.Get(gocontext.Background(), client.ObjectKey{Namespace: kvm.Namespace, Name: kvm.Name}, vm)).To(Succeed())
		return vm
	}

	getSecret := func(cl client.Client, name string) *corev1.Secret {
		secret := &corev1.Secret{}
		ExpectWithOffset(1, cl.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: name}, secret)).To(Succeed())
		return secret
	}

	It("does not provision the machine when the KubevirtIPAM feature gate is disabled", func() {
		setup()
		reconciler.IPAMEnabled = false

		out, err := reconciler.reconcileNormal(machineContext)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(ctrl.Result{}))

		for _, conditionType := range []string{infrav1.IPAddressClaimedCondition, infrav1.VMProvisionedCondition} {
			cond := conditions.Get(kubevirtMachine, conditionType)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(infrav1.IPAMFeatureDisabledReason))
			Expect(cond.Message).To(ContainSubstring("--feature-gates=KubevirtIPAM=true"))
		}
		Expect(recorder.Events).To(Receive(And(ContainSubstring("Warning"), ContainSubstring(infrav1.IPAMFeatureDisabledReason))))

		// Nothing is claimed, read or created.
		claims := &ipamv1.IPAddressClaimList{}
		Expect(c.List(gocontext.Background(), claims)).To(Succeed())
		Expect(claims.Items).To(BeEmpty())
		err = c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: kubevirtMachine.Name}, &kubevirtv1.VirtualMachine{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("claims an address and does not create the VM until it is allocated", func() {
		setup()

		out, err := reconciler.reconcileNormal(machineContext)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(ctrl.Result{RequeueAfter: 30 * time.Second}))

		claim := &ipamv1.IPAddressClaim{}
		Expect(c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: claimName}, claim)).To(Succeed())
		Expect(metav1.IsControlledBy(claim, kubevirtMachine)).To(BeTrue())
		Expect(claim.Spec.ClusterName).To(Equal(clusterName))

		err = c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: kubevirtMachine.Name}, &kubevirtv1.VirtualMachine{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		cond := conditions.Get(kubevirtMachine, infrav1.IPAddressClaimedCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(infrav1.WaitingForIPAddressReason))
		Expect(cond.Message).To(ContainSubstring(claimName))
	})

	It("creates the VM with a static network configuration once the address is allocated", func() {
		setup(boundClaimAndAddress(kubevirtMachine, "192.0.2.10")...)

		out, err := reconciler.reconcileNormal(machineContext)
		Expect(err).NotTo(HaveOccurred())
		// Waiting for the VMI to come online.
		Expect(out).To(Equal(ctrl.Result{RequeueAfter: 20 * time.Second}))
		Expect(conditions.IsTrue(kubevirtMachine, infrav1.IPAddressClaimedCondition)).To(BeTrue())

		vm := getVM(c, kubevirtMachine)
		Expect(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress).To(Equal("02:00:c0:00:02:0a"))
		drive := cloudInitConfigDrive(vm)
		Expect(drive).NotTo(BeNil())
		Expect(drive.UserDataSecretRef).To(Equal(&corev1.LocalObjectReference{Name: bootstrapSecretName + "-userdata"}))
		Expect(drive.NetworkDataSecretRef).To(Equal(&corev1.LocalObjectReference{Name: kubevirtMachine.Name + "-networkdata"}))

		userData := getSecret(c, bootstrapSecretName+"-userdata")
		Expect(userData.Data).To(HaveLen(1))
		Expect(userData.Data).To(HaveKey("userdata"))

		networkDataSecret := getSecret(c, kubevirtMachine.Name+"-networkdata")
		Expect(networkDataSecret.Labels).To(HaveKeyWithValue(clusterv1.ClusterNameLabel, clusterName))
		Expect(networkDataSecret.Data).To(HaveLen(1))
		networkData := map[string]interface{}{}
		Expect(json.Unmarshal(networkDataSecret.Data["networkdata"], &networkData)).To(Succeed())
		Expect(string(networkDataSecret.Data["networkdata"])).To(ContainSubstring(`"ip_address":"192.0.2.10"`))
		Expect(string(networkDataSecret.Data["networkdata"])).To(ContainSubstring(`"gateway":"192.0.2.1"`))
	})

	It("renders cloud-init network config v2 for the noCloud data source", func() {
		kubevirtMachine.Spec.CloudInit = &infrav1.CloudInitSpec{DataSource: infrav1.CloudInitDataSourceNoCloud}
		setup(boundClaimAndAddress(kubevirtMachine, "192.0.2.10")...)

		_, err := reconciler.reconcileNormal(machineContext)
		Expect(err).NotTo(HaveOccurred())

		noCloud := cloudInitNoCloud(getVM(c, kubevirtMachine))
		Expect(noCloud).NotTo(BeNil())
		Expect(noCloud.NetworkDataSecretRef).To(Equal(&corev1.LocalObjectReference{Name: kubevirtMachine.Name + "-networkdata"}))

		networkDataSecret := getSecret(c, kubevirtMachine.Name+"-networkdata")
		Expect(string(networkDataSecret.Data["networkdata"])).To(ContainSubstring("version: 2"))
		Expect(string(networkDataSecret.Data["networkdata"])).To(ContainSubstring("192.0.2.10/24"))
	})

	It("writes the network data and the VM to the infra cluster only", func() {
		external := fake.NewClientBuilder().WithScheme(testing.SetupScheme()).Build()
		setupWithInfraClient(external, boundClaimAndAddress(kubevirtMachine, "192.0.2.10")...)

		_, err := reconciler.reconcileNormal(machineContext)
		Expect(err).NotTo(HaveOccurred())

		vm := getVM(external, kubevirtMachine)
		Expect(cloudInitConfigDrive(vm).NetworkDataSecretRef).To(Equal(&corev1.LocalObjectReference{Name: kubevirtMachine.Name + "-networkdata"}))
		Expect(string(getSecret(external, kubevirtMachine.Name+"-networkdata").Data["networkdata"])).To(ContainSubstring(`"ip_address":"192.0.2.10"`))

		// Nothing is written to the management cluster but the claims.
		err = c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: kubevirtMachine.Name + "-networkdata"}, &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	Context("when machines share a bootstrap data secret", func() {
		It("gives each machine the network data rendered from its own addresses", func() {
			machine2, kubevirtMachine2 := peerMachine()
			objects := append(boundClaimAndAddress(kubevirtMachine, "192.0.2.10"), boundClaimAndAddress(kubevirtMachine2, "192.0.2.11")...)
			setup(append(objects, machine2, kubevirtMachine2)...)
			machineContext2 := newMachineContext(machine2, kubevirtMachine2)

			_, err := reconciler.reconcileNormal(machineContext)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.reconcileNormal(machineContext2)
			Expect(err).NotTo(HaveOccurred())
			// Reconciling the first machine again must not pick up the second one's data either.
			_, err = reconciler.reconcileNormal(machineContext)
			Expect(err).NotTo(HaveOccurred())

			for kvm, expected := range map[*infrav1.KubevirtMachine]struct{ ip, mac string }{
				kubevirtMachine:  {ip: "192.0.2.10", mac: "02:00:c0:00:02:0a"},
				kubevirtMachine2: {ip: "192.0.2.11", mac: "02:00:c0:00:02:0b"},
			} {
				vm := getVM(c, kvm)
				Expect(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress).To(Equal(expected.mac))
				drive := cloudInitConfigDrive(vm)
				Expect(drive.UserDataSecretRef.Name).To(Equal(bootstrapSecretName + "-userdata"))
				Expect(drive.NetworkDataSecretRef.Name).To(Equal(kvm.Name + "-networkdata"))

				networkData := string(getSecret(c, drive.NetworkDataSecretRef.Name).Data["networkdata"])
				Expect(networkData).To(ContainSubstring(`"ip_address":"` + expected.ip + `"`))
				Expect(networkData).To(ContainSubstring(`"ethernet_mac_address":"` + expected.mac + `"`))
			}
			Expect(getSecret(c, bootstrapSecretName+"-userdata").Data).NotTo(HaveKey("networkdata"))
		})

		It("gives each machine its own spec.cloudInit network data", func() {
			machine2, kubevirtMachine2 := peerMachine()
			kubevirtMachine.Spec.Network = nil
			kubevirtMachine.Spec.CloudInit = &infrav1.CloudInitSpec{NetworkData: ptr.To(`{"networks":[{"ip_address":"192.0.2.10"}]}`)}
			kubevirtMachine2.Spec.Network = nil
			kubevirtMachine2.Spec.CloudInit = &infrav1.CloudInitSpec{NetworkData: ptr.To(`{"networks":[{"ip_address":"192.0.2.11"}]}`)}
			setup(machine2, kubevirtMachine2)
			machineContext2 := newMachineContext(machine2, kubevirtMachine2)

			_, err := reconciler.reconcileNormal(machineContext)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.reconcileNormal(machineContext2)
			Expect(err).NotTo(HaveOccurred())

			for _, kvm := range []*infrav1.KubevirtMachine{kubevirtMachine, kubevirtMachine2} {
				drive := cloudInitConfigDrive(getVM(c, kvm))
				Expect(drive.NetworkDataSecretRef.Name).To(Equal(kvm.Name + "-networkdata"))
				Expect(getSecret(c, drive.NetworkDataSecretRef.Name).Data["networkdata"]).To(Equal([]byte(*kvm.Spec.CloudInit.NetworkData)))
			}
		})
	})

	It("keeps the address until the VM is gone, then releases it and deletes the network data secret", func() {
		vmObj := &kubevirtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: kubevirtMachine.Name, Namespace: kubevirtMachine.Namespace},
		}
		networkDataSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: kubevirtMachine.Name + "-networkdata", Namespace: kubevirtMachine.Namespace},
			Data:       map[string][]byte{"networkdata": []byte("{}")},
		}
		setup(append(boundClaimAndAddress(kubevirtMachine, "192.0.2.10"), vmObj, networkDataSecret)...)

		// First pass deletes the VM but keeps the claim.
		out, err := reconciler.reconcileDelete(machineContext)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(ctrl.Result{RequeueAfter: 5 * time.Second}))
		claim := &ipamv1.IPAddressClaim{}
		Expect(c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: claimName}, claim)).To(Succeed())
		Expect(claim.Finalizers).To(ContainElement(ipam.ClaimProtectionFinalizer))
		Expect(kubevirtMachine.Finalizers).To(ContainElement(infrav1.MachineFinalizer))
		err = c.Get(gocontext.Background(), types.NamespacedName{Namespace: kubevirtMachine.Namespace, Name: networkDataSecret.Name}, &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		// Second pass finds the VM gone and releases the claim.
		out, err = reconciler.reconcileDelete(machineContext)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(ctrl.Result{}))
		err = c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: claimName}, &ipamv1.IPAddressClaim{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(kubevirtMachine.Finalizers).NotTo(ContainElement(infrav1.MachineFinalizer))
	})

	It("stops with IPAddressInvalid when a pool allocates an IPv6 address", func() {
		setup(boundClaimAndAddress(kubevirtMachine, "2001:db8::10")...)

		out, err := reconciler.reconcileNormal(machineContext)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(ctrl.Result{}), "terminal, not requeued")

		cond := conditions.Get(kubevirtMachine, infrav1.IPAddressClaimedCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(infrav1.IPAddressInvalidReason))
		Expect(cond.Message).To(ContainSubstring("only IPv4 pools are supported for now"))
		Expect(recorder.Events).To(Receive(And(ContainSubstring("Warning"), ContainSubstring(infrav1.IPAddressInvalidReason))))

		err = c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: kubevirtMachine.Name}, &kubevirtv1.VirtualMachine{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		err = c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: kubevirtMachine.Name + "-networkdata"}, &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("releases the claims it owns even when spec.network no longer lists them", func() {
		// A KubevirtMachine whose spec.network was removed before the field became immutable.
		claims := boundClaimAndAddress(kubevirtMachine, "192.0.2.10")
		kubevirtMachine.Spec.Network = nil
		setup(claims...)

		out, err := reconciler.reconcileDelete(machineContext)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(ctrl.Result{}))
		err = c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: claimName}, &ipamv1.IPAddressClaim{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(kubevirtMachine.Finalizers).NotTo(ContainElement(infrav1.MachineFinalizer))
	})

	It("writes only the userdata when spec.network is unset", func() {
		kubevirtMachine.Spec.Network = nil
		setup()

		_, err := reconciler.reconcileNormal(machineContext)
		Expect(err).NotTo(HaveOccurred())

		secret := getSecret(c, bootstrapSecretName+"-userdata")
		Expect(secret.Data).To(HaveLen(1))
		Expect(secret.Data).To(HaveKey("userdata"))
		err = c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: kubevirtMachine.Name + "-networkdata"}, &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(conditions.Get(kubevirtMachine, infrav1.IPAddressClaimedCondition)).To(BeNil())

		claims := &ipamv1.IPAddressClaimList{}
		Expect(c.List(gocontext.Background(), claims)).To(Succeed())
		Expect(claims.Items).To(BeEmpty())
	})
})
