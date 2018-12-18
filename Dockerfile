FROM  debian:stretch-slim

ADD ./bin/sriov /usr/src/sriov-cni/bin/sriov

WORKDIR /

LABEL io.k8s.display-name="SR-IOV CNI"

ADD ./images/entrypoint.sh /

ENTRYPOINT ["/entrypoint.sh"]
