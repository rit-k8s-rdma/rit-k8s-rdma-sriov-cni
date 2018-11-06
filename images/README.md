## Dockerfile build

This is used for distribution of SR-IOV CNI binary in a Docker image.

`build_docker.sh` script is available for building the SR-IOV CNI docker image from the `./images` directory.

```
$ ./build_docker.sh -h
./build_docker [COMMAND]
Examples:
./build_docker image           To build the image
./build_docker local           To build the binary localy
./build_docker manifest        Modify manifest
```
It allows to build docker image for SR-IOV CNI for supported docker architectures (amd64 ppc64le), also it can build a the binary files localy:

```
$ ./build_docker.sh local
Building plugins
  sriov
  fixipam
```
To build image for specific architecture you need to specify the architecture (ARCH) and the version (VERSION):

```
$ ARCH=amd64 VERSION=0.6 ./build_docker.sh image
```
To make the manifest, which build the image for all supported architectures and annotates the manifest for each image architecture, you need to specify the version (VERSION):

```
VERSION=0.6 ./build_docker.sh manifest
---

## Daemonset deployment

You may wish to deploy SR-IOV CNI as a daemonset, you can do so by starting with the example Daemonset shown here:

```
$ kubectl create -f ./images/sriov-cni-daemonset.yaml
```

Note: The likely best practice here is to build your own image given the Dockerfile, and then push it to your preferred registry, and change the `image` fields in the Daemonset YAML to reference that image.

---

### Development notes

Example docker run command:

```
$ docker run -it -v /opt/cni/bin/:/host/opt/cni/bin/ --entrypoint=/bin/bash nfvpe/sriov-cni
```

Originally inspired by and is a portmanteau of the [Flannel daemonset](https://github.com/coreos/flannel/blob/master/Documentation/kube-flannel.yml), the [Calico Daemonset](https://github.com/projectcalico/calico/blob/master/v2.0/getting-started/kubernetes/installation/hosted/k8s-backend-addon-manager/calico-daemonset.yaml), and the [Calico CNI install bash script](https://github.com/projectcalico/cni-plugin/blob/be4df4db2e47aa7378b1bdf6933724bac1f348d0/k8s-install/scripts/install-cni.sh#L104-L153).
