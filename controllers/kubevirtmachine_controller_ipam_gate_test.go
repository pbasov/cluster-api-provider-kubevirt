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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
)

// TestManagerStartsWithoutIPAM starts a manager running the KubevirtMachine controller against a real
// API server, as a user that has the base manager role only, or the base role and the role of the
// config/components/ipam component. With the KubevirtIPAM gate disabled the manager must start
// without the IPAM API and without any ipam.cluster.x-k8s.io RBAC; with the gate enabled it needs
// both, which the negative cases show the test would notice.
//
// It needs the envtest control plane binaries (KUBEBUILDER_ASSETS, see setup-envtest) and is skipped
// without them.
func TestManagerStartsWithoutIPAM(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run `setup-envtest use -p path` and export its output to run this test")
	}

	capiCRDs := capiCRDDirectory(t)
	env := &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths: []string{
				filepath.Join("..", "config", "crd", "bases"),
				filepath.Join(capiCRDs, "cluster.x-k8s.io_clusters.yaml"),
				filepath.Join(capiCRDs, "cluster.x-k8s.io_machines.yaml"),
			},
			ErrorIfPathMissing: true,
		},
	}
	adminConfig, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start the test control plane: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("failed to stop the test control plane: %v", err)
		}
	})

	adminClient, err := client.New(adminConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	baseRole := clusterRoleFromFile(t, adminClient, filepath.Join("..", "config", "rbac", "role.yaml"))
	ipamRole := clusterRoleFromFile(t, adminClient, filepath.Join("..", "config", "components", "ipam", "role.yaml"))
	for _, rule := range baseRole.Rules {
		for _, group := range rule.APIGroups {
			if group == ipamv1.GroupVersion.Group {
				t.Fatalf("the base manager role must not grant access to %s: %v", group, rule)
			}
		}
	}

	type testCase struct {
		name     string
		ipam     bool
		roles    []string
		wantFail bool
	}
	run := func(t *testing.T, tc testCase) {
		userName := "capk-" + strings.ReplaceAll(tc.name, " ", "-")
		user, err := env.AddUser(envtest.User{Name: userName}, adminConfig)
		if err != nil {
			t.Fatal(err)
		}
		for _, role := range tc.roles {
			binding := &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{GenerateName: role + "-"},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: userName}},
			}
			if err := adminClient.Create(gocontext.Background(), binding); err != nil {
				t.Fatal(err)
			}
		}

		err = startKubevirtMachineManager(t, user.Config(), tc.ipam)
		switch {
		case tc.wantFail && err == nil:
			t.Fatal("expected the manager to fail to start, it ran")
		case !tc.wantFail && err != nil:
			t.Fatalf("expected the manager to run, it failed: %v", err)
		case err != nil:
			t.Logf("manager failed as expected: %v", err)
		}
	}

	// The IPAM API is not served yet.
	for _, tc := range []testCase{
		{name: "gate disabled without the IPAM API", roles: []string{baseRole.Name}},
		{name: "gate enabled without the IPAM API", ipam: true, roles: []string{baseRole.Name, ipamRole.Name}, wantFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) { run(t, tc) })
	}

	// The IPAM API is served, as with Cluster API core v1.5 and later.
	if _, err := envtest.InstallCRDs(adminConfig, envtest.CRDInstallOptions{
		Paths: []string{
			filepath.Join(capiCRDs, "ipam.cluster.x-k8s.io_ipaddressclaims.yaml"),
			filepath.Join(capiCRDs, "ipam.cluster.x-k8s.io_ipaddresses.yaml"),
		},
		ErrorIfPathMissing: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []testCase{
		{name: "gate disabled without IPAM RBAC", roles: []string{baseRole.Name}},
		{name: "gate enabled without IPAM RBAC", ipam: true, roles: []string{baseRole.Name}, wantFail: true},
		{name: "gate enabled with IPAM RBAC", ipam: true, roles: []string{baseRole.Name, ipamRole.Name}},
	} {
		t.Run(tc.name, func(t *testing.T) { run(t, tc) })
	}
}

// startKubevirtMachineManager runs a manager with the KubevirtMachine controller, set up the way
// main.go sets it up, for long enough to see whether its caches sync. It returns nil if the manager
// was still running at the end.
func startKubevirtMachineManager(t *testing.T, config *rest.Config, ipamEnabled bool) error {
	t.Helper()

	managerScheme := runtime.NewScheme()
	addToScheme := []func(*runtime.Scheme) error{scheme.AddToScheme, infrav1.AddToScheme, clusterv1.AddToScheme}
	if ipamEnabled {
		addToScheme = append(addToScheme, ipamv1.AddToScheme)
	}
	for _, f := range addToScheme {
		if err := f(managerScheme); err != nil {
			t.Fatal(err)
		}
	}

	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:                 managerScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := gocontext.WithTimeout(gocontext.Background(), 15*time.Second)
	defer cancel()
	reconciler := &KubevirtMachineReconciler{Client: mgr.GetClient(), IPAMEnabled: ipamEnabled}
	if err := reconciler.SetupWithManager(ctx, mgr, controller.Options{
		// Every case sets up a controller of the same name in a manager of its own.
		SkipNameValidation: ptr.To(true),
		CacheSyncTimeout:   5 * time.Second,
	}, logr.Discard()); err != nil {
		t.Fatal(err)
	}

	return mgr.Start(ctx)
}

func clusterRoleFromFile(t *testing.T, c client.Client, path string) *rbacv1.ClusterRole {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(data, role); err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	if err := c.Create(gocontext.Background(), role); err != nil {
		t.Fatal(err)
	}
	return role
}

// capiCRDDirectory returns the directory of the Cluster API CRDs in the module cache.
func capiCRDDirectory(t *testing.T) string {
	t.Helper()

	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/cluster-api").Output()
	if err != nil {
		t.Fatalf("failed to locate the sigs.k8s.io/cluster-api module: %v", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "config", "crd", "bases")
}
