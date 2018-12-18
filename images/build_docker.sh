#!/bin/bash

set -e

VALID_CMDS=(image manifest local)
VALID_DOCKER_ARCH=(amd64 ppc64le)

declare -A MACHINEARCH2DOCKER_ARCH
MACHINEARCH2DOCKER_ARCH[x86_64]=amd64
MACHINEARCH2DOCKER_ARCH[ppc64le]=ppc64le

declare -A DOCKER2MACHINEARCH
DOCKER2MACHINEARCH[amd64]=x86_64
DOCKER2MACHINEARCH[ppc64le]=ppc64le

GIT_VER=$(git rev-list -1 HEAD)

MACHINE_ARCH=$(uname -m)
IMAGE_NAME=k8s-sriov-cni
REPO_NAME=rdma
QEMU_DOCKER_IMAGE=multiarch/qemu-user-static:register
DEBIAN_DOCKER_IMAGE=debian:stretch-slim
input_cmd=""

function usage_help()
{
	echo "./build [COMMAND]"
	echo "Examples:"
	echo "./build image           To build the image"
	echo "./build local           To build the binary without container image"
	echo "./build manifest        Build image for all supported architectures and modify manifest"
}

function check_for_help()
{
	case $1 in
	        "-h" | "--help")
	                usage_help
	                exit 0
	                ;;
	esac
}

function validate_machine_arch()
{
	if [ ! ${MACHINEARCH2DOCKER_ARCH[$1]+_} ] ; then
		echo "Unspported Arch $1"
		usage_help
		exit 1
	fi
}

function validate_input_cmd()
{
	valid_cmd=""
	in_cmd=$1
	for n in "${VALID_CMDS[@]}"; do
		if [ "$in_cmd" = "$n" ]; then
			valid_cmd=$in_cmd
		fi
	done

	if [ -z $valid_cmd ]; then
		echo "Given command $in_cmd is invalid"
		usage_help
		exit 1
	fi
}

function build_image()
{
	if [ -z $ARCH ]; then
		echo "ARCH is not set"
		echo "using machine ARCH"
		ARCH=${MACHINEARCH2DOCKER_ARCH[$MACHINE_ARCH]}
	fi
	if [ -z ${DOCKER2MACHINEARCH[$ARCH]+_} ]; then
		echo "Unsupported ARCH $ARCH"
		echo "Supported ARCHs (${VALID_DOCKER_ARCH[@]})"
                exit 1
	fi
	if [[ $(docker images -q "${REPO_NAME}/${IMAGE_NAME}-${ARCH}:${VERSION}") != "" ]]; then
		echo "Image ${REPO_NAME}/${IMAGE_NAME}-${ARCH}:${VERSION} already exists"
		return
	fi
	echo "Building image $IMAGE_NAME for ARCH $ARCH:"
	if [[ $(docker images -q "${DEBIAN_DOCKER_IMAGE}") != "" ]]; then
		docker image rm "$DEBIAN_DOCKER_IMAGE"
	fi
	../build.sh
	if [ ${ARCH} != ${MACHINEARCH2DOCKER_ARCH[$MACHINE_ARCH]} ] ; then
		# different arch
		if [[ $(docker images -q "${QEMU_DOCKER_IMAGE}") == "" ]]; then
			docker run --rm --privileged "${QEMU_DOCKER_IMAGE}"
		fi
		if [ ! -d ~/cli ]; then
			git clone  https://github.com/docker/cli.git ~/cli
		fi
		if [ ! -f ~/cli/build/docker-linux-${ARCH} ]; then
			current_path=$(pwd)
			cd ~/cli
			make -f docker.Makefile cross
			cd $current_path
		fi
		cp ../Dockerfile ../Dockerfile.${ARCH}
		interpreter=$(sed -n '2,2p' /proc/sys/fs/binfmt_misc/qemu-${ARCH} | cut -b 13-)
		sed -i '/^FROM debian:stretch-slim/a COPY ./qemu/qemu-'${DOCKER2MACHINEARCH[$ARCH]}'-static '${interpreter} ../Dockerfile.${ARCH}
		if [ ! -d ~/qemu ]; then
			mkdir -p ~/qemu
		fi
		if [ ! -f ~/qemu/qemu-${ARCH}-static ]; then
			current_path=$(pwd)
			cd ~/qemu
			wget -N https://github.com/multiarch/qemu-user-static/releases/download/v2.9.1-1/${MACHINE_ARCH}_qemu-${ARCH}-static.tar.gz
			tar -xvf ${MACHINE_ARCH}_qemu-${ARCH}-static.tar.gz
			cd $current_path
		fi
		mkdir -p ../qemu
		cp ~/qemu/qemu-${ARCH}-static ../qemu
		cp "../qemu/qemu-${ARCH}-static" $interpreter
		~/cli/build/docker-linux-${ARCH} build -f ../Dockerfile.${ARCH} --pull --platform ${ARCH} -t ${REPO_NAME}/${IMAGE_NAME}-${ARCH}:${VERSION} ..
		rm ../Dockerfile.${ARCH}
		rm -rf ../qemu
	else
		# same arch
		docker build -f ../Dockerfile --pull --platform ${ARCH} -t ${REPO_NAME}/${IMAGE_NAME}-${ARCH}:${VERSION} ..
	fi
	echo "Finished building image successfully"
}

function execute_cmd()
{
	case "$input_cmd" in
	"image")
		build_image
	;;
	"local")
		../build.sh
		echo "Finished building binary successfully"
	;;

	"manifest")
		echo "Building image for ARCHs x86_64 and ppc64le"
		for n in ${VALID_DOCKER_ARCH[@]}
		do
			ARCH=$n
			build_image
		done
		docker manifest create rdma/${IMAGE_NAME}:${VERSION} rdma/${IMAGE_NAME}-amd64:${VERSION} rdma/${IMAGE_NAME}-ppc64le:${VERSION}
		docker manifest annotate rdma/$IMAGE_NAME:${VERSION} rdma/$IMAGE_NAME-x86_64:${VERSION} --os linux --arch amd64
		docker manifest annotate rdma/$IMAGE_NAME:${VERSION} rdma/$IMAGE_NAME-ppc64le:${VERSION} --os linux --arch ppc64le
	;;
	esac
}

check_for_help $1

if [[ ! -z $1 ]]; then
	validate_input_cmd $1
fi

if [[ -z $VERSION && $1 != "local" ]]; then
	echo 'VERSION is not set'
	exit 1
fi

validate_machine_arch $MACHINE_ARCH

if [ $# -lt 1 ]; then
	input_cmd=image
else
	input_cmd=$1
fi

execute_cmd
