# Deploy SaxML Multihost with LWS on GKE

In this example, we will use LeaderWorkerSet to deploy a multi-host inference instance on TPUs with Saxml. You could use the steps [here](https://cloud.google.com/kubernetes-engine/docs/tutorials/tpu-multihost-saxml#before-you-begin) to setup your TPU clusters, node pools, GCS bucket, and configure workload access. 

## Install LeaderWorkerSet

Follow the step-by-step guide on how to install LWS. [View installation guide](https://github.com/kubernetes-sigs/lws/blob/main/docs/setup/install.md)

## Conceptual View

![image](https://github.com/kubernetes-sigs/lws/assets/86417275/2c4a9abd-f988-4f2e-ad3f-c7b03f0b6485)

## Deploy ConfigMap with model configuration
The ConfigMap contains what model it is, and the checkpoint that will be used. If the model information on the ConfigMap is updated, the HTTP Server will unpublish the model that was loaded, and publish a model that reflects the new model information.

Apply the `configmap.yaml` manifest:

```shell
kubectl apply -f configmap.yaml
```


## Deploy LeaderWorkerSet Deployment

We use LeaderWorkerSet to deploy two Saxml model replicas on two TPU multi-host pod slices. 
On the leader pod, the leader pod runs the Sax admin and the http servers, while the workers run the Sax model servers.

Replace the GCS_BUCKET with the name of your GCS bucket and apply the `lws.yaml` manifest:
```shell
kubectl apply -f lws.yaml
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

Apply the `service.yaml` manifest

```shell
kubectl apply -f service.yaml
```

Wait for the service to have an external IP address assigned

```shell
kubectl get svc
```

The output should be similar to the following
```shell
NAME           TYPE         CLUSTER-IP      EXTERNAL-IP      PORT(S)       AGE
saxml  LoadBalancer   10.68.56.41    10.182.0.187   8888:31876/TCP   56s

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