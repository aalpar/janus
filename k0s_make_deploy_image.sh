#!/bin/bash
IMG_BASENAME=janus-controller
IMG_REPONAME=aalpar
IMG_VERSION=$(cat VERSION)
IMG=${IMG_REPONAME}/${IMG_BASENAME}:${IMG_VERSION}
make docker-build IMG=${IMG} && \
	docker save ${IMG_BASENAME}:latest | \
	sudo /usr/local/bin/k0s ctr --address /run/k0s/containerd.sock images import --base-name ${IMG_BASENAME} -

