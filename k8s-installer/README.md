## k8s-sriov-cni-installer

This is installer for Kubernetes SRIOV CNI networking plugin.

## How to use it?

1. Install Kubernetes

2. Apply SRIOV cni configuration before deploying any Pods

```
kubectl apply -f https://cdn.rawgit.com/Mellanox/sriov-cni/37883b34/k8s-installer/k8s-sriov-cni-installer.yaml
```
This installs necessary binaries and sriov configuration file.

Configuration file is located at /etc/cni/net.d/10-sriov-cni.conf

3. User must modify /etc/cni/net.d/10-sriov-cni.conf to configure PF netdevices name(s), IP addresses.

