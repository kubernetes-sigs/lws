# How to use LeaderWorkerSet Features
Below are some examples to use different features of LeaderWorkerSet.

Deploy a LeaderWorkerSet with 3 groups and 4 workers in each group. You can find an example [here](lws.yaml)

## Multi template for leader/worker pods

LWS support using different templates for leader and worker pods. You could find the example [here](lws-multi-template.yaml), 
leader pod's spec is specified in leaderTemplate, and worker pods' spec is specified in workerTemplate.

## Restart Policy

You could specify the RestartPolicy to define the failure handling schematics for the pod group.
By default, only failed pods will be automatically restarted. When the RestartPolicy is set to RecreateGroupOnRestart, it will recreate 
the groups on container/pods restarts. All the worker pods will be recreated after the new leader pod is started.
You could find an example [here](lws-restart-policy.yaml)

## Horizontal Pod Autoscaler (HPA)

LWS expose a scale endpoint for HPA to trigger scaling. An example HPA yaml for LWS can be found [here](horizontal-pod-autoscaler.yaml)


## Exclusive Placement
LeaderWorkerSet supports exclusive placement through node affinity/anti-affinity where pods in the same group will be scheduled on the same topology domain (ex, GKE node pool), but on different nodes. In the context of GKE node pool, this ensures 1:1 pod-to-node mapping and 1:1 podgroup-to-nodepool mapping.
This feature can be enabled by adding the exclusive topology annotation **leaderworkerset.sigs.k8s.io/exclusive-topology:** as shown [here](lws-exclusive-placement.yaml)