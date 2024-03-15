# Deploy SaxML Multihost with LWS on GKE

Follow the steps on this [guide](https://cloud.google.com/kubernetes-engine/docs/tutorials/tpu-multihost-saxml#before-you-begin) upto the deploy saxml step
to create GKE cluster, two TPU nodepools, and configure kubectl with the cluster. 

## Install LeaderWorkerSet

Follow the step-by-step guide on how to install LWS. [View installation guide](https://github.com/kubernetes-sigs/lws/blob/main/docs/setup/install.md)


## Deploy ConfigMap with model configuration

Apply the `configmaplws.yaml` manifest:

```shell
kubectl apply -f configmaplws.yaml
```


## Deploy LeaderWorkerSet Deployment

Apply the `leader-worker-set.yaml` manifest:
```shell
kubectl apply -f leader-worker-set.yaml
```

Verify the status of the SaxML Deployment
```shell
kubectl get pods
```

Should get an output similar to this
```shell
NAME                             READY   STATUS    RESTARTS      AGE
saxml-multi-host-0                3/3     Running   0          3m12s
saxml-multi-host-0-1              1/1     Running   0          3m12s
saxml-multi-host-0-2              1/1     Running   0          3m12s
saxml-multi-host-0-3              1/1     Running   0          3m12s
saxml-multi-host-0-4              1/1     Running   0          3m12s
saxml-multi-host-0-5              1/1     Running   0          3m12s
saxml-multi-host-0-6              1/1     Running   0          3m12s
saxml-multi-host-0-7              1/1     Running   0          3m12s
saxml-multi-host-1                3/3     Running   0          3m12s
saxml-multi-host-1-1              1/1     Running   0          3m12s
saxml-multi-host-1-2              1/1     Running   0          3m12s
saxml-multi-host-1-3              1/1     Running   0          3m12s
saxml-multi-host-1-4              1/1     Running   0          3m12s
saxml-multi-host-1-5              1/1     Running   0          3m12s
saxml-multi-host-1-6              1/1     Running   0          3m12s
saxml-multi-host-1-7              1/1     Running   0          3m12s

```

# Use SaxML

## Deploy LoadBalancer

Apply the `lws-lb.yaml` manifest

```shell
kubectl apply -f lws-lb.yaml
```

Wait for the service to have an external IP address assigned

```shell
kubectl get svc
```

The output should be similar to the following
```shell
NAME           TYPE         CLUSTER-IP      EXTERNAL-IP      PORT(S)       AGE
lws-http-lb  LoadBalancer   10.68.56.41    10.182.0.187   8888:31876/TCP   56s

```

## Serve the Model

Retrieve the load balancer IP address for SaxML
```shell
LB_IP=$(kubectl get svc sax-http-lb -o jsonpath='{.status.loadBalancer.ingress[*].ip}')
PORT="8888"
```

Serve a request
```shell
curl --request POST \
 --header "Content-type: application/json" \
-s ${LB_IP}:${PORT}/generate --data \
'{
  "model": "/sax/test/spmd",
  "query": "How many days are in a week?"
}'
```