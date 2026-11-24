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
		c                client.Client
	)

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
				Name: "lan",
				AddressesFromPools: []corev1.TypedLocalObjectReference{{
					APIGroup: ptr.To("ipam.cluster.x-k8s.io"),
					Kind:     "InClusterIPPool",
					Name:     "pool",
				}},
				Nameservers: []string{"192.0.2.53"},
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

	setup := func(objects ...client.Object) {
		objects = append([]client.Object{cluster, kubevirtCluster, machine, kubevirtMachine, sshKeySecret, bootstrapSecret}, objects...)
		c = fake.NewClientBuilder().WithScheme(testing.SetupScheme()).WithObjects(objects...).WithStatusSubresource(objects...).Build()
		machineContext = &context.MachineContext{
			Context:         logr.NewContext(gocontext.Background(), GinkgoLogr),
			Cluster:         cluster,
			KubevirtCluster: kubevirtCluster,
			Machine:         machine,
			KubevirtMachine: kubevirtMachine,
			Logger:          ctrl.Log.WithName("test"),
		}
		reconciler = KubevirtMachineReconciler{
			Client:          c,
			InfraCluster:    infraClusterMock,
			WorkloadCluster: workloadclustermock.NewMockWorkloadCluster(gomock.NewController(GinkgoT())),
			MachineFactory:  kubevirt.DefaultMachineFactory{},
		}
		infraClusterMock.EXPECT().GenerateInfraClusterClient(gomock.Any(), gomock.Any(), gomock.Any()).Return(c, kubevirtMachine.Namespace, nil).AnyTimes()
	}

	boundClaimAndAddress := func() []client.Object {
		claim := &ipamv1.IPAddressClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:       claimName,
				Finalizers: []string{ipam.ClaimProtectionFinalizer},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: infrav1.GroupVersion.String(),
					Kind:       "KubevirtMachine",
					Name:       kubevirtMachine.Name,
					UID:        kubevirtMachine.UID,
					Controller: ptr.To(true),
				}},
			},
			Spec:   ipamv1.IPAddressClaimSpec{ClusterName: clusterName, PoolRef: ipamv1.IPPoolReference{APIGroup: "ipam.cluster.x-k8s.io", Kind: "InClusterIPPool", Name: "pool"}},
			Status: ipamv1.IPAddressClaimStatus{AddressRef: ipamv1.IPAddressReference{Name: claimName}},
		}
		address := &ipamv1.IPAddress{
			ObjectMeta: metav1.ObjectMeta{Name: claimName},
			Spec: ipamv1.IPAddressSpec{
				ClaimRef: ipamv1.IPAddressClaimReference{Name: claimName},
				PoolRef:  ipamv1.IPPoolReference{APIGroup: "ipam.cluster.x-k8s.io", Kind: "InClusterIPPool", Name: "pool"},
				Address:  "192.0.2.10",
				Prefix:   ptr.To[int32](24),
				Gateway:  "192.0.2.1",
			},
		}
		return []client.Object{claim, address}
	}

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
		setup(boundClaimAndAddress()...)

		out, err := reconciler.reconcileNormal(machineContext)
		Expect(err).NotTo(HaveOccurred())
		// Waiting for the VMI to come online.
		Expect(out).To(Equal(ctrl.Result{RequeueAfter: 20 * time.Second}))
		Expect(conditions.IsTrue(kubevirtMachine, infrav1.IPAddressClaimedCondition)).To(BeTrue())

		vm := &kubevirtv1.VirtualMachine{}
		Expect(c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: kubevirtMachine.Name}, vm)).To(Succeed())
		Expect(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress).To(Equal("02:00:c0:00:02:0a"))
		var drive *kubevirtv1.CloudInitConfigDriveSource
		for _, v := range vm.Spec.Template.Spec.Volumes {
			if v.Name == "cloudinitvolume" {
				drive = v.CloudInitConfigDrive
			}
		}
		Expect(drive).NotTo(BeNil())
		Expect(drive.NetworkDataSecretRef).To(Equal(&corev1.LocalObjectReference{Name: bootstrapSecretName + "-userdata"}))

		secret := &corev1.Secret{}
		Expect(c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: bootstrapSecretName + "-userdata"}, secret)).To(Succeed())
		Expect(secret.Data).To(HaveKey("userdata"))
		Expect(secret.Data).To(HaveKey("networkdata"))
		networkData := map[string]interface{}{}
		Expect(json.Unmarshal(secret.Data["networkdata"], &networkData)).To(Succeed())
		Expect(string(secret.Data["networkdata"])).To(ContainSubstring(`"ip_address":"192.0.2.10"`))
		Expect(string(secret.Data["networkdata"])).To(ContainSubstring(`"gateway":"192.0.2.1"`))

		// The rendered configuration is kept in a per-machine Secret owned by the KubevirtMachine,
		// which a change to it maps back to.
		perMachine := &corev1.Secret{}
		Expect(c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: kubevirtMachine.Name + "-networkdata"}, perMachine)).To(Succeed())
		Expect(perMachine.Data["networkdata"]).To(Equal(secret.Data["networkdata"]))
		Expect(metav1.IsControlledBy(perMachine, kubevirtMachine)).To(BeTrue())
		Expect(reconciler.SecretToKubevirtMachines(gocontext.Background(), perMachine)).To(ConsistOf(
			ctrl.Request{NamespacedName: client.ObjectKeyFromObject(kubevirtMachine)}))
	})

	It("renders cloud-init network config v2 for the noCloud data source", func() {
		kubevirtMachine.Spec.CloudInit = &infrav1.CloudInitSpec{DataSource: infrav1.CloudInitDataSourceNoCloud}
		setup(boundClaimAndAddress()...)

		_, err := reconciler.reconcileNormal(machineContext)
		Expect(err).NotTo(HaveOccurred())

		vm := &kubevirtv1.VirtualMachine{}
		Expect(c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: kubevirtMachine.Name}, vm)).To(Succeed())
		var noCloud *kubevirtv1.CloudInitNoCloudSource
		for _, v := range vm.Spec.Template.Spec.Volumes {
			if v.Name == "cloudinitvolume" {
				noCloud = v.CloudInitNoCloud
			}
		}
		Expect(noCloud).NotTo(BeNil())
		Expect(noCloud.NetworkDataSecretRef).To(Equal(&corev1.LocalObjectReference{Name: bootstrapSecretName + "-userdata"}))

		secret := &corev1.Secret{}
		Expect(c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: bootstrapSecretName + "-userdata"}, secret)).To(Succeed())
		Expect(string(secret.Data["networkdata"])).To(ContainSubstring("version: 2"))
		Expect(string(secret.Data["networkdata"])).To(ContainSubstring("192.0.2.10/24"))
	})

	It("keeps the address until the VM is gone, then releases it", func() {
		vmObj := &kubevirtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: kubevirtMachine.Name, Namespace: kubevirtMachine.Namespace},
		}
		setup(append(boundClaimAndAddress(), vmObj)...)

		// First pass deletes the VM but keeps the claim.
		out, err := reconciler.reconcileDelete(machineContext)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(ctrl.Result{RequeueAfter: 5 * time.Second}))
		claim := &ipamv1.IPAddressClaim{}
		Expect(c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: claimName}, claim)).To(Succeed())
		Expect(claim.Finalizers).To(ContainElement(ipam.ClaimProtectionFinalizer))
		Expect(kubevirtMachine.Finalizers).To(ContainElement(infrav1.MachineFinalizer))

		// Second pass finds the VM gone and releases the claim.
		out, err = reconciler.reconcileDelete(machineContext)
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

		secret := &corev1.Secret{}
		Expect(c.Get(gocontext.Background(), client.ObjectKey{Namespace: kubevirtMachine.Namespace, Name: bootstrapSecretName + "-userdata"}, secret)).To(Succeed())
		Expect(secret.Data).To(HaveLen(1))
		Expect(secret.Data).To(HaveKey("userdata"))
		Expect(conditions.Get(kubevirtMachine, infrav1.IPAddressClaimedCondition)).To(BeNil())

		claims := &ipamv1.IPAddressClaimList{}
		Expect(c.List(gocontext.Background(), claims)).To(Succeed())
		Expect(claims.Items).To(BeEmpty())
	})
})
