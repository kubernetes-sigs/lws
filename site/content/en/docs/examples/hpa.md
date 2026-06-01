---
title: "Horizontal Pod Autoscaler (HPA)"
linkTitle: "HPA"
weight: 5
description: >
  An example of using Horizontal Pod Autoscaler with LeaderWorkerSet
---

LeaderWorkerSet supports Horizontal Pod Autoscaler (HPA) through its scale subresource. This allows you to automatically scale the number of replica groups based on resource utilization metrics like CPU or memory.

## How HPA Works with LeaderWorkerSet

When using HPA with LeaderWorkerSet:

- HPA monitors **leader pods** only (not worker pods)
- The `hpaPodSelector` in LeaderWorkerSet status helps HPA identify which pods to monitor
- Scaling affects the number of **replica groups**, not individual pods within a group
- Each replica group (leader + workers) is scaled as a unit

## Prerequisites

Before setting up HPA, ensure you have:

### 1. Install Metrics Server

HPA requires metrics-server to be running in your cluster to collect CPU and memory metrics. Here are the detailed installation steps:

#### Step 1: Download metrics-server configuration file

```shell
wget https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml -O metrics-server.yaml
```

#### Step 2: Modify configuration file

metrics-server requires certificate validation to access kubelet, but Kind clusters use self-signed certificates. Edit the `metrics-server.yaml` file and add the `--kubelet-insecure-tls` parameter to the deployment's `args` section:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    k8s-app: metrics-server
  name: metrics-server
  namespace: kube-system
spec:
  # ... other configurations remain unchanged
  template:
    spec:
      containers:
      - args:
        - --cert-dir=/tmp
        - --secure-port=4443
        - --kubelet-preferred-address-types=InternalIP,ExternalIP,Hostname
        - --kubelet-use-node-status-port
        - --metric-resolution=15s
        - --kubelet-insecure-tls  # Add this parameter to skip certificate verification
        image: registry.k8s.io/metrics-server/metrics-server:v0.8.0
        # If you encounter image download issues, you can use domestic mirrors:
        # image: registry.cn-hangzhou.aliyuncs.com/rainux/metrics-server:v0.6.4
```

#### Step 3: Deploy metrics-server

```shell
kubectl apply -f metrics-server.yaml
```

#### Step 4: Verify metrics-server is running

```shell
# Check metrics-server pod status
kubectl get pods -n kube-system | grep metrics

# Output should be similar to:
# metrics-server-xxxxxxxxx-xxxxx   1/1     Running   0          2m
```

#### Step 5: Verify metrics API availability

```shell
# Check if metrics API is available
kubectl get --raw /apis/metrics.k8s.io/v1beta1 | python3 -m json.tool
```

The output should include nodes and pods resources:
```json
{
    "kind": "APIResourceList",
    "apiVersion": "v1",
    "groupVersion": "metrics.k8s.io/v1beta1",
    "resources": [
        {
            "name": "nodes",
            "singularName": "",
            "namespaced": false,
            "kind": "NodeMetrics",
            "verbs": ["get", "list"]
        },
        {
            "name": "pods",
            "singularName": "",
            "namespaced": true,
            "kind": "PodMetrics",
            "verbs": ["get", "list"]
        }
    ]
}
```

#### Step 6: Test metrics collection

```shell
# Test node metrics collection
kubectl top nodes

# Test pod metrics collection
kubectl top pods
```

If the commands execute successfully and display CPU and memory usage, metrics-server is installed successfully.

#### Troubleshooting

If you encounter issues, check the metrics-server logs:

```shell
kubectl logs -n kube-system deployment/metrics-server
```

Common issues and solutions:

- **Certificate errors**: Ensure the `--kubelet-insecure-tls` parameter is added
- **Network issues**: Check if metrics-server can access kubelet's port 10250
- **Image download failures**: Use domestic mirror sources or configure image accelerator
- **API unavailable**: Wait 2-3 minutes for metrics-server to fully start, then retry

### 2. Configure Resource Requests

**Pods must define CPU/memory resource requests**: HPA calculates scaling based on the percentage of resource requests, so all containers must set `resources.requests`.

### 3. LeaderWorkerSet Deployment

**A running LeaderWorkerSet deployment**: Ensure LeaderWorkerSet is properly deployed and in Running status.

## Example: CPU-based Autoscaling

Here's a complete example showing how to set up HPA with LeaderWorkerSet:

### Step 1: Deploy LeaderWorkerSet

First, create a LeaderWorkerSet with proper resource requests:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  replicas: 2
  leaderWorkerTemplate:
    size: 3
    leaderTemplate:
      spec:
        containers:
        - name: leader
          image: nginx:1.14.2
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: 500m
              memory: 256Mi
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: nginx:1.14.2
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: 500m
              memory: 256Mi
```

### Step 2: Create HPA

Apply the HPA configuration targeting the LeaderWorkerSet:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: lws-hpa
spec:
  minReplicas: 2
  maxReplicas: 8
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 50
  scaleTargetRef:
    apiVersion: leaderworkerset.x-k8s.io/v1
    kind: LeaderWorkerSet
    name: leaderworkerset-sample
```

### Step 3: Verify HPA Setup

Check that HPA is properly configured:

```shell
kubectl get hpa
```

Output should look similar to:
```shell
NAME      REFERENCE                         TARGETS   MINPODS   MAXPODS   REPLICAS   AGE
lws-hpa   LeaderWorkerSet/leaderworkerset-sample   1%/50%    2         8         2          2m
```

Check the LeaderWorkerSet status for HPA pod selector:

```shell
kubectl get leaderworkerset leaderworkerset-sample -o jsonpath='{.status.hpaPodSelector}'
```

## Testing Autoscaling

To test the autoscaling behavior, you can generate load on the leader pods:

### Generate CPU Load

⚠️ **Important**: HPA only monitors LeaderWorkerSet's leader pods, so you must generate CPU load in these pods to trigger scaling.

```shell
# Generate high CPU load in both leader pods
kubectl exec leaderworkerset-sample-0 -- /bin/sh -c "for i in \$(seq 1 4); do yes > /dev/null & done"
kubectl exec leaderworkerset-sample-1 -- /bin/sh -c "for i in \$(seq 1 4); do yes > /dev/null & done"
```

### Monitor Scaling

Run the following commands in multiple terminals simultaneously to observe the scaling process:

```shell
# Monitor HPA status
kubectl get hpa -w

# Monitor LeaderWorkerSet replica changes
kubectl get leaderworkerset leaderworkerset-sample -w

# Monitor pods status
kubectl get pods -l leaderworkerset.sigs.k8s.io/name=leaderworkerset-sample -w

# Check detailed status and events
kubectl describe hpa lws-hpa
kubectl top pods -l leaderworkerset.sigs.k8s.io/name=leaderworkerset-sample
```

**Expected Scaling Behavior**

After generating load, you should see within 2-3 minutes:
1. CPU usage rises from 1% to 300%+
2. HPA status changes from `1%/50%` to `300%/50%`
3. LeaderWorkerSet replicas gradually increase to 8
4. New leader and worker pods are created

**Stop Load Testing**

```shell
# Restart leader pods to clear CPU load (nginx container doesn't have pkill command)
kubectl delete pod leaderworkerset-sample-0 leaderworkerset-sample-1
```

After clearing the load, you will see within 5-10 minutes:
1. CPU usage decreases to 0%
2. HPA waits for the stabilization window before starting to scale down
3. LeaderWorkerSet replicas gradually decrease to 2

## Advanced HPA Configuration

### Memory-based Scaling

You can also configure HPA to scale based on memory utilization:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: lws-memory-hpa
spec:
  minReplicas: 1
  maxReplicas: 5
  metrics:
  - type: Resource
    resource:
      name: memory
      target:
        type: Utilization
        averageUtilization: 70
  scaleTargetRef:
    apiVersion: leaderworkerset.x-k8s.io/v1
    kind: LeaderWorkerSet
    name: leaderworkerset-sample
```

### Multi-metric Configuration

Configure HPA to use multiple metrics:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: lws-multi-metric-hpa
spec:
  minReplicas: 2
  maxReplicas: 10
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 50
  - type: Resource
    resource:
      name: memory
      target:
        type: Utilization
        averageUtilization: 70
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 300
      policies:
      - type: Percent
        value: 50
        periodSeconds: 60
    scaleUp:
      stabilizationWindowSeconds: 0
      policies:
      - type: Percent
        value: 100
        periodSeconds: 15
  scaleTargetRef:
    apiVersion: leaderworkerset.x-k8s.io/v1
    kind: LeaderWorkerSet
    name: leaderworkerset-sample
```

## Important Considerations

### Scaling Behavior
- **Group-level scaling**: HPA scales entire replica groups (leader + workers) as units when scaling up/down
- **Leader pod monitoring**: HPA only monitors leader pods, ensure leaders represent the load of the entire group
- **Resource requests**: All containers must set `resources.requests`, HPA calculates utilization based on this

### Best Practices
1. **Set appropriate resource requests**: Set reasonable CPU/memory request values
2. **Monitor leader metrics**: Ensure leader pods reflect actual load conditions
3. **Use stabilization windows**: Use behavior configuration to prevent rapid scaling oscillations
4. **Thorough testing**: Validate behavior under different load patterns

### Troubleshooting
Common issues:
- **HPA shows "unknown"**: Check metrics-server status and pods resource requests
- **Scaling not triggered**: Confirm load is in the correct pods (leader pods)
- **Oscillation issues**: Adjust stabilization parameters in behavior section

## Cleanup

To remove the HPA and LeaderWorkerSet:

```shell
kubectl delete hpa lws-hpa
kubectl delete leaderworkerset leaderworkerset-sample
```