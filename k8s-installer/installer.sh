#!/bin/bash

cp -f /bin/sriov /host-cni-bin/
cp -f /bin/fixipam /host-cni-bin/

cp -f /installer/10-sriov-cni.conf /host-cni-etc/
