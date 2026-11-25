# Static guest addresses from Cluster API IPAM pools

By default a KubeVirt guest gets its address from DHCP, whether that comes from KubeVirt's
in-pod DHCP server on the pod network or from a DHCP server on a secondary network. When the
VM is attached to a secondary network that has no DHCP server, or must not use one, CAPK can
claim the addresses from a [Cluster API IPAM](https://cluster-api.sigs.k8s.io/developer/providers/contracts/ipam)
pool and configure them statically in the guest.

## Prerequisites

- A Cluster API IPAM provider, for example the
  [in-cluster IPAM provider](https://github.com/kubernetes-sigs/cluster-api-ipam-provider-in-cluster).
  Cluster API core installs the `IPAddressClaim` and `IPAddress` CRDs.
- The `KubevirtIPAM` feature gate of the CAPK manager (alpha, disabled by default), and RBAC on
  `ipam.cluster.x-k8s.io` for the manager. Both come with the kustomize component
  `config/components/ipam`: add it to the kustomization that builds the provider
  (`components: [../components/ipam]` in `config/default`). For an installation done by
  `clusterctl`, apply the `ClusterRole` and `ClusterRoleBinding` of that component (with the
  `capk-` prefix and the `capk-system` namespace) and add `--feature-gates=KubevirtIPAM=true` to
  the arguments of `capk-controller-manager`. A repeated `--feature-gates` flag adds to the gates
  set by earlier ones.

Without the gate the manager neither watches nor reads any `ipam.cluster.x-k8s.io` resource and
starts without the IPAM API or RBAC. A `KubevirtMachine` with `spec.network` is then not
provisioned: `IPAddressClaimed` and `VMProvisioned` are `False` with the reason
`IPAMFeatureDisabled`, and a Warning event says how to enable the gate. The machine is not
requeued; it is reconciled again when it or its `Machine` changes, and when the manager restarts,
for example with the gate enabled.

## How it works

Set `spec.network` on the `KubevirtMachineTemplate` (or `KubevirtMachine`). For every interface
listed there, CAPK:

1. creates one `IPAddressClaim` per entry of `addressesFromPools`, named
   `<machine>-<interface>-<index>`, controlled by the `KubevirtMachine` and protected by the
   `kubevirtmachine.infrastructure.cluster.x-k8s.io/ip-claim-protection` finalizer;
2. waits until the IPAM provider binds every claim to an `IPAddress`. The VM is not created before
   that. Progress is reported in the `IPAddressClaimed` condition, which is part of the `Ready`
   summary: while claims are pending its reason is `WaitingForIPAddress`, and the message names
   each pending claim, its pool, the time it was created, and the reason the IPAM provider set on
   the claim, if any. The contract does not require providers to report an exhausted pool, and
   the in-cluster provider sets no claim condition in that case (it only logs it), so a claim
   that stays pending usually means that the pool has no free address. `VMProvisioned` is `False`
   with the same reason meanwhile;
3. pins a MAC address on the interface, unless the template already sets one. The MAC address is
   derived from the first address of the interface (`02:00` followed by the four octets), so it
   stays the same as long as the address does;
4. renders the static configuration (addresses, one default route, DNS servers), matched by that
   MAC address, in the format of `spec.cloudInit.dataSource`: an OpenStack `network_data.json`
   document for `configDrive` (the default), cloud-init network config version 2 for `noCloud`.
   It is written to the secret `<kubevirtmachine>-networkdata` next to the VM in the infra
   cluster, under the key `networkdata`, and referenced from the cloud-init volume. Each machine
   has its own secret, also when machines share a bootstrap data secret; the
   `<bootstrap secret>-userdata` secret holds only the user data. (`spec.cloudInit.networkData`
   and `networkDataSecretRef` are delivered through the same per-machine secret.) Once the VM
   exists, it owns the secret, so the secret is garbage collected with VMs deleted without CAPK.
   cloud-init applies it before the network comes up, so the guest never runs DHCP on the
   interface;
5. reports the claimed addresses in `status.addresses`, without relying on the qemu guest agent.
   They come first, followed by the addresses the VMI reports (IPv4 and IPv6), without
   duplicates. The SSH bootstrap check keeps using the address the VMI reports on its first
   interface, and uses the first claimed address only when the VMI reports none there (a bridged
   interface without the guest agent).

When the `KubevirtMachine` is deleted, the claims it controls are released only after the VM is
gone, so a replacement machine is never handed an address that is still in use. The claims are
found by ownership, not from `spec.network`. The network data secret is deleted with the machine.

The address belongs to the `KubevirtMachine`: it survives VM restarts, stop/start, live migration
and in-place updates, and is returned to the pool when the machine is deleted. A rollout that
deletes a machine before creating its replacement (for example `maxSurge: 0`) hands the freed
address to the replacement with the reference in-cluster IPAM provider, which allocates the
lowest free address. A rollout that surges needs spare addresses in the pool.

`spec.cloudInit.networkData` and `spec.cloudInit.networkDataSecretRef` give every machine of a
template the same network configuration, which suits DHCP or per-machine templates. `spec.network`
gives each machine its own addresses from a shared template, so a `MachineDeployment` or a
control plane with several replicas can use static addresses. The two cannot be combined.

## Example

The example uses the [in-cluster IPAM provider](https://github.com/kubernetes-sigs/cluster-api-ipam-provider-in-cluster)
and a bridge network without IPAM, so neither KubeVirt nor the network hands out addresses.

```yaml
apiVersion: ipam.cluster.x-k8s.io/v1alpha2
kind: InClusterIPPool
metadata:
  name: my-cluster-nodes
spec:
  addresses:
    - 192.0.2.10-192.0.2.14
  prefix: 24
  gateway: 192.0.2.1
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: node-lan
spec:
  config: '{"cniVersion":"1.0.0","type":"bridge","bridge":"br0","ipam":{}}'
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: KubevirtMachineTemplate
metadata:
  name: my-cluster-control-plane
spec:
  template:
    spec:
      network:
        interfaces:
          - name: lan
            addressesFromPools:
              - apiGroup: ipam.cluster.x-k8s.io
                kind: InClusterIPPool
                name: my-cluster-nodes
            nameservers:
              - 192.0.2.53
      virtualMachineTemplate:
        spec:
          runStrategy: Always
          template:
            spec:
              domain:
                devices:
                  interfaces:
                    - name: lan
                      bridge: {}
                  disks:
                    - name: containervolume
                      disk:
                        bus: virtio
                memory:
                  guest: 4Gi
              networks:
                - name: lan
                  multus:
                    networkName: node-lan
              volumes:
                - name: containervolume
                  containerDisk:
                    image: quay.io/capk/ubuntu-2404-container-disk:v1.32.1
```

## Notes and limitations

- Only IPv4 is supported for now: one IPv4 address per interface, from one pool
  (`addressesFromPools` accepts one entry), and up to three IPv4 nameservers. An IPv6 address
  allocated by a pool stops the machine: `IPAddressClaimed` and `VMProvisioned` are `False` with
  the reason `IPAddressInvalid` and a Warning event is emitted; the machine is not requeued. The
  API is shaped for dual stack: `addressesFromPools` stays a list, so a second (IPv6) pool can be
  allowed later by relaxing the limit, and the nameserver validation can be relaxed to IP
  addresses.
- The rendered network data is the guest's whole network configuration. Interfaces of the
  template that are not listed in `spec.network` get no configuration in the guest (no DHCP).
- Every interface of `spec.network` must exist in the VM template; the webhook rejects a
  `KubevirtMachine` or `KubevirtMachineTemplate` that names another one on creation.
- The guest image must use cloud-init with the datasource set in `spec.cloudInit.dataSource`.
- The control plane endpoint is not derived from the pool. With a secondary network as the only
  interface, set `KubevirtCluster.spec.controlPlaneEndpoint` to an address served from inside the
  guests (for example a keepalived virtual IP).
- `spec.network` is immutable on a `KubevirtMachine`, like `spec.cloudInit`, and the
  `KubevirtMachineTemplate` spec is immutable as a whole; change it by rolling out a new template.
- The derived MAC address (`02:00` + IPv4 octets) is locally administered. If KubeMacPool manages
  the namespace, make sure its range does not overlap, or set the MAC in the template.
- `clusterctl move` moves the claims with their machines (owner references). Pools are not owned
  by a cluster; label them with `clusterctl.cluster.x-k8s.io/move` to move them along.
- `<kubevirtmachine>-networkdata` is a reserved secret name in the VM namespace.
