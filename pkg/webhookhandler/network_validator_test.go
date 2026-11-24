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

package webhookhandler

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/annotationfilter"
)

var _ = Describe("spec.network interface validation", func() {
	var (
		codec            runtime.Codec
		machineValidator *infraSecretRefValidator
		templateHandler  *kubevirtMachineTemplateHandler
	)

	BeforeEach(func() {
		s := scheme.Scheme
		Expect(v1alpha1.AddToScheme(s)).To(Succeed())
		codec = serializer.NewCodecFactory(s).LegacyCodec(v1alpha1.GroupVersion)
		machineValidator = &infraSecretRefValidator{decoder: admission.NewDecoder(s), excludedAnnotations: annotationfilter.New(nil)}
		templateHandler = &kubevirtMachineTemplateHandler{decoder: admission.NewDecoder(s), excludedAnnotations: annotationfilter.New(nil)}
	})

	machineSpec := func(networkInterface string) v1alpha1.KubevirtMachineSpec {
		return v1alpha1.KubevirtMachineSpec{
			VirtualMachineTemplate: v1alpha1.VirtualMachineTemplateSpec{
				Spec: kubevirtv1.VirtualMachineSpec{Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
					Spec: kubevirtv1.VirtualMachineInstanceSpec{Domain: kubevirtv1.DomainSpec{Devices: kubevirtv1.Devices{
						Interfaces: []kubevirtv1.Interface{{Name: "default"}, {Name: "lan"}},
					}}},
				}},
			},
			Network: &v1alpha1.NetworkSpec{Interfaces: []v1alpha1.NetworkInterfaceSpec{{
				Name:               networkInterface,
				AddressesFromPools: []v1alpha1.IPPoolReference{{APIGroup: "ipam.cluster.x-k8s.io", Kind: "InClusterIPPool", Name: "pool"}},
			}}},
		}
	}

	DescribeTable("KubevirtMachine", func(operation admissionv1.Operation, networkInterface string, allowed bool) {
		machine := &v1alpha1.KubevirtMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Namespace: "test-ns"},
			Spec:       machineSpec(networkInterface),
		}

		res := machineValidator.HandleMachine(context.Background(), newMachineRequest(operation, machine, codec))
		Expect(res.Allowed).To(Equal(allowed))
		if !allowed {
			Expect(res.Result.Message).To(ContainSubstring(`spec.network.interfaces[0].name: interface "wan" is not defined`))
		}
	},
		Entry("allows creating a machine whose interface is in the template", admissionv1.Create, "lan", true),
		Entry("rejects creating a machine whose interface is not in the template", admissionv1.Create, "wan", false),
		// Updates are not checked: spec.network is immutable, and the controller must still be able
		// to remove the finalizer of a machine admitted before the check existed.
		Entry("allows updating a machine admitted before the check", admissionv1.Update, "wan", true),
	)

	DescribeTable("KubevirtMachineTemplate", func(networkInterface string, allowed bool) {
		template := &v1alpha1.KubevirtMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "test-ns"},
			Spec: v1alpha1.KubevirtMachineTemplateSpec{
				Template: v1alpha1.KubevirtMachineTemplateResource{Spec: machineSpec(networkInterface)},
			},
		}

		res := templateHandler.Handle(context.Background(), newRequest(admissionv1.Create, template, nil, codec))
		Expect(res.Allowed).To(Equal(allowed))
		if !allowed {
			Expect(res.Result.Message).To(ContainSubstring(`interface "wan" is not defined`))
		}
	},
		Entry("allows creating a template whose interface is in the VM template", "lan", true),
		Entry("rejects creating a template whose interface is not in the VM template", "wan", false),
	)
})
