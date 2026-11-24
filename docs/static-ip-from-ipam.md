# Static guest addresses from Cluster API IPAM pools

By default a KubeVirt guest gets its address from DHCP, whether that comes from KubeVirt's
in-pod DHCP server on the pod network or from a DHCP server on a secondary network. When the
VM is attached to a secondary network that has no DHCP server, or must not use one, CAPK can
claim the addresses from a [Cluster API IPAM](https://cluster-api.sigs.k8s.io/developer/providers/contracts/ipam)
pool and configure them statically in the guest.

## How it works

Set `spec.network` on the `KubevirtMachineTemplate` (or `KubevirtMachine`). For every interface
listed there, CAPK:

1. creates one `IPAddressClaim` per entry of `addressesFromPools`, named
   `<machine>-<interface>-<index>`, controlled by the `KubevirtMachine` and protected by the
   `kubevirtmachine.infrastructure.cluster.x-k8s.io/ip-claim-protection` finalizer;
2. waits until the IPAM provider binds every claim to an `IPAddress`. Progress is reported in the
   `IPAddressClaimed` condition, including the `PoolExhausted` reason when the pool is full. The
   VM is not created before that;
3. pins a MAC address on the interface, unless the template already sets one. The MAC address is
   derived from the first address of the interface (`02:00` followed by the four octets), so it
   stays the same as long as the address does;
4. renders the static configuration (addresses, one default route, DNS servers), matched by that
   MAC address, in the format of `spec.cloudInit.dataSource`: an OpenStack `network_data.json`
   document for `configDrive` (the default), cloud-init network config version 2 for `noCloud`.
   It is stored in a per-machine secret `<machine>-networkdata`, owned by the `KubevirtMachine`,
   and delivered exactly like a user-provided `spec.cloudInit.networkDataSecretRef`: copied into
   the `<bootstrap secret>-userdata` secret under the key `networkdata` and referenced from the
   cloud-init volume. cloud-init applies it before the network comes up, so the guest never runs
   DHCP on the interface;
5. reports the claimed addresses in `status.addresses`, without relying on the qemu guest agent.

When the `KubevirtMachine` is deleted, the claims are released only after the VM is gone, so a
replacement machine is never handed an address that is still in use.

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

- Only IPv4 pools are supported.
- The guest image must use cloud-init with the datasource set in `spec.cloudInit.dataSource`.
- The control plane endpoint is not derived from the pool. With a secondary network as the only
  interface, set `KubevirtCluster.spec.controlPlaneEndpoint` to an address served from inside the
  guests (for example a keepalived virtual IP).
- `spec.network` is read when the VM is created; changing it on an existing machine has no effect
  until the machine is replaced.
