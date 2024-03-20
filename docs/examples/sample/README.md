# LeaderWorkerSet Features
LeaderWorkerSet supports the following features.

## Multi template for leader/worker pods

Different templates for leaders and workers can be specified as seen [here](lws-multi-template.yaml),
Leader template is an optional field. If not specified, it will default to the worker template.

## Restart Policy

There exists an API field called RestartPolicy to define the failure handling schematics for the pod group.
On default, only failed pods will be restarted. When it is set to RecreateGroupOnRestart, it will recreate 
the groups on container/pods restart. All the worker pods will be stopped before the new leader pod is started.
This field as set as shown [here](lws-restart-policy.yaml)

## Horizontal Pod Autoscaler (HPA)

A horizontal pod autoscaler can be deployed alongside LeaderWorkerSet. An example yaml can be found [here](horizontal-pod-autoscaler.yaml)


## Exclusive Placement
LeaderWorkerSet can force exclusive 1:1 placement so that each Leader Worker group is scheduled on the same node pool 
by adding the exclusive topology annotation as shown [here](lws-exclusive-placement.yaml)
