#!/bin/bash

export CWD=$(pwd)
function cleanup {
    if [ $USE_EXISTING_CLUSTER == 'false' ] 
    then 
        $KIND delete cluster --name $KIND_CLUSTER_NAME
    fi
    (cd $CWD/config/manager && $KUSTOMIZE edit set image controller=gcr.io/k8s-staging-lws/lws:main)
}
function startup {
    if [ $USE_EXISTING_CLUSTER == 'false' ] 
    then 
        $KIND create cluster --name $KIND_CLUSTER_NAME --image $E2E_KIND_VERSION
    fi
}
function kind_load {
    $KIND load docker-image $IMAGE_TAG --name $KIND_CLUSTER_NAME
}
function lws_deploy {
    cd $CWD/config/manager && $KUSTOMIZE edit set image controller=$IMAGE_TAG  
    $KUSTOMIZE build $CWD/test/e2e/config | $KUBECTL apply --server-side -f -
}
trap cleanup EXIT
startup
kind_load
lws_deploy
$GINKGO -v $CWD/test/e2e/...