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

package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/yaml"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
)

const (
	// celPerCallLimit and celCostBudget are the limits the API server applies to the validation
	// rules of a custom resource (k8s.io/apiserver/pkg/apis/cel).
	celPerCallLimit = 1000000
	celCostBudget   = 10000000
)

// crdValidator applies the OpenAPI schema and the validation rules of a generated CRD the way the
// API server does, so that the markers on the API types are tested without an API server.
type crdValidator struct {
	schema     apiservervalidation.SchemaValidator
	structural *structuralschema.Structural
}

func newCRDValidator(t *testing.T, file string) *crdValidator {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", file))
	if err != nil {
		t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, crd); err != nil {
		t.Fatal(err)
	}
	internal := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, internal, nil); err != nil {
		t.Fatal(err)
	}
	schemaValidator, _, err := apiservervalidation.NewSchemaValidator(internal)
	if err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(internal)
	if err != nil {
		t.Fatal(err)
	}
	return &crdValidator{schema: schemaValidator, structural: structural}
}

// validate returns the errors of creating obj, or of updating old to obj when old is not nil.
func (v *crdValidator) validate(t *testing.T, obj, old runtime.Object) field.ErrorList {
	t.Helper()

	if old == nil {
		return v.validateUnstructured(toUnstructured(t, obj), nil)
	}
	return v.validateUnstructured(toUnstructured(t, obj), toUnstructured(t, old))
}

// validateUnstructured is validate for objects that the API types cannot express, such as
// objects without a required field.
func (v *crdValidator) validateUnstructured(newObj, oldObj map[string]interface{}) field.ErrorList {
	var errs field.ErrorList
	var old interface{}
	if oldObj == nil {
		errs = apiservervalidation.ValidateCustomResource(nil, newObj, v.schema)
	} else {
		old = oldObj
		errs = apiservervalidation.ValidateCustomResourceUpdate(nil, newObj, oldObj, v.schema)
	}
	celErrs, _ := cel.NewValidator(v.structural, true, celPerCallLimit).Validate(context.Background(), nil, v.structural, newObj, old, celCostBudget)
	return append(errs, celErrs...)
}

func toUnstructured(t *testing.T, o runtime.Object) map[string]interface{} {
	t.Helper()

	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func expectValid(t *testing.T, errs field.ErrorList) {
	t.Helper()
	if len(errs) > 0 {
		t.Errorf("expected no validation errors, got %v", errs)
	}
}

func expectInvalid(t *testing.T, errs field.ErrorList, message string) {
	t.Helper()
	if !strings.Contains(errs.ToAggregate().Error(), message) {
		t.Errorf("expected a validation error containing %q, got %v", message, errs)
	}
}

func newKubevirtMachine(network *infrav1.NetworkSpec, cloudInit *infrav1.CloudInitSpec) *infrav1.KubevirtMachine {
	return &infrav1.KubevirtMachine{
		Spec: infrav1.KubevirtMachineSpec{
			VirtualMachineTemplate: infrav1.VirtualMachineTemplateSpec{
				Spec: kubevirtv1.VirtualMachineSpec{Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{}},
			},
			Network:   network,
			CloudInit: cloudInit,
		},
	}
}

func lanNetwork(pool string) *infrav1.NetworkSpec {
	return &infrav1.NetworkSpec{Interfaces: []infrav1.NetworkInterfaceSpec{{
		Name: "lan",
		AddressesFromPools: []infrav1.IPPoolReference{{
			APIGroup: "ipam.cluster.x-k8s.io",
			Kind:     "InClusterIPPool",
			Name:     pool,
		}},
	}}}
}

func TestKubevirtMachinePoolReferenceRequiresAllFields(t *testing.T) {
	v := newCRDValidator(t, "infrastructure.cluster.x-k8s.io_kubevirtmachines.yaml")

	expectValid(t, v.validate(t, newKubevirtMachine(lanNetwork("pool"), nil), nil))

	for _, fieldName := range []string{"apiGroup", "kind", "name"} {
		t.Run("missing "+fieldName, func(t *testing.T) {
			obj := toUnstructured(t, newKubevirtMachine(lanNetwork("pool"), nil))
			network := obj["spec"].(map[string]interface{})["network"].(map[string]interface{})
			iface := network["interfaces"].([]interface{})[0].(map[string]interface{})
			pool := iface["addressesFromPools"].([]interface{})[0].(map[string]interface{})
			delete(pool, fieldName)
			expectInvalid(t, v.validateUnstructured(obj, nil), "spec.network.interfaces[0].addressesFromPools[0]."+fieldName+": Required value")

			pool[fieldName] = ""
			expectInvalid(t, v.validateUnstructured(obj, nil), "spec.network.interfaces[0].addressesFromPools[0]."+fieldName+": Invalid value")
		})
	}
}

func TestKubevirtMachineNetworkIsIPv4Only(t *testing.T) {
	v := newCRDValidator(t, "infrastructure.cluster.x-k8s.io_kubevirtmachines.yaml")

	withNameservers := func(nameservers ...string) *infrav1.NetworkSpec {
		network := lanNetwork("pool")
		network.Interfaces[0].Nameservers = nameservers
		return network
	}
	expectValid(t, v.validate(t, newKubevirtMachine(withNameservers("192.0.2.53", "198.51.100.53", "203.0.113.53"), nil), nil))

	t.Run("second pool for an interface", func(t *testing.T) {
		network := lanNetwork("pool")
		network.Interfaces[0].AddressesFromPools = append(network.Interfaces[0].AddressesFromPools,
			infrav1.IPPoolReference{APIGroup: "ipam.cluster.x-k8s.io", Kind: "InClusterIPPool", Name: "ipv6-pool"})
		expectInvalid(t, v.validate(t, newKubevirtMachine(network, nil), nil), "spec.network.interfaces[0].addressesFromPools: Too many")
	})
	t.Run("IPv6 nameserver", func(t *testing.T) {
		expectInvalid(t, v.validate(t, newKubevirtMachine(withNameservers("2001:db8::53"), nil), nil), "spec.network.interfaces[0].nameservers[0]")
	})
	t.Run("hostname as nameserver", func(t *testing.T) {
		expectInvalid(t, v.validate(t, newKubevirtMachine(withNameservers("dns.example.com"), nil), nil), "spec.network.interfaces[0].nameservers[0]")
	})
	t.Run("more nameservers than a resolver uses", func(t *testing.T) {
		expectInvalid(t, v.validate(t, newKubevirtMachine(withNameservers("192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"), nil), nil),
			"spec.network.interfaces[0].nameservers: Too many")
	})
}

func TestKubevirtMachineNetworkCannotBeCombinedWithCloudInitNetworkData(t *testing.T) {
	v := newCRDValidator(t, "infrastructure.cluster.x-k8s.io_kubevirtmachines.yaml")

	expectValid(t, v.validate(t, newKubevirtMachine(lanNetwork("pool"), &infrav1.CloudInitSpec{DataSource: infrav1.CloudInitDataSourceNoCloud}), nil))
	expectInvalid(t, v.validate(t, newKubevirtMachine(lanNetwork("pool"), &infrav1.CloudInitSpec{NetworkData: ptr.To("version: 2")}), nil),
		"network cannot be combined with cloudInit.networkData or cloudInit.networkDataSecretRef")
	expectInvalid(t, v.validate(t, newKubevirtMachine(lanNetwork("pool"), &infrav1.CloudInitSpec{NetworkDataSecretRef: &corev1.LocalObjectReference{Name: "nd"}}), nil),
		"network cannot be combined with cloudInit.networkData or cloudInit.networkDataSecretRef")
}

func TestKubevirtMachineNetworkIsImmutable(t *testing.T) {
	v := newCRDValidator(t, "infrastructure.cluster.x-k8s.io_kubevirtmachines.yaml")

	for _, tc := range []struct {
		name     string
		old, new *infrav1.NetworkSpec
		message  string
	}{
		{name: "adding network", old: nil, new: lanNetwork("pool"), message: "Network presence is immutable"},
		{name: "removing network", old: lanNetwork("pool"), new: nil, message: "Network presence is immutable"},
		{name: "changing the pool", old: lanNetwork("pool"), new: lanNetwork("other-pool"), message: "Network is immutable"},
		{name: "adding an interface", old: lanNetwork("pool"), new: func() *infrav1.NetworkSpec {
			n := lanNetwork("pool")
			n.Interfaces = append(n.Interfaces, infrav1.NetworkInterfaceSpec{Name: "other", AddressesFromPools: n.Interfaces[0].AddressesFromPools})
			return n
		}(), message: "Network is immutable"},
		{name: "unrelated update with network", old: lanNetwork("pool"), new: lanNetwork("pool")},
		{name: "unrelated update without network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := newKubevirtMachine(tc.old, nil)
			updated := newKubevirtMachine(tc.new, nil)
			updated.Spec.ProviderID = ptr.To("kubevirt://updated")

			errs := v.validate(t, updated, old)
			if tc.message == "" {
				expectValid(t, errs)
			} else {
				expectInvalid(t, errs, tc.message)
			}
		})
	}
}

func TestKubevirtMachineCloudInitIsImmutable(t *testing.T) {
	v := newCRDValidator(t, "infrastructure.cluster.x-k8s.io_kubevirtmachines.yaml")

	noCloud := &infrav1.CloudInitSpec{DataSource: infrav1.CloudInitDataSourceNoCloud}
	configDrive := &infrav1.CloudInitSpec{DataSource: infrav1.CloudInitDataSourceConfigDrive}
	expectInvalid(t, v.validate(t, newKubevirtMachine(nil, noCloud), newKubevirtMachine(nil, nil)), "CloudInit presence is immutable")
	expectInvalid(t, v.validate(t, newKubevirtMachine(nil, noCloud), newKubevirtMachine(nil, configDrive)), "CloudInit is immutable")
	expectValid(t, v.validate(t, newKubevirtMachine(nil, noCloud), newKubevirtMachine(nil, noCloud)))
}
