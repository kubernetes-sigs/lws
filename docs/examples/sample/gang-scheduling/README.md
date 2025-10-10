# Enabling Gang Scheduling with different Schedulers

This document provides guidance about how to enable gang scheduling capabilities in LeaderWorkerSet with different schedulers.

## Supported Schedulers

| Custom Scheduler Community | GitHub ID       | Email                           |
| -------------------------- |-----------------|---------------------------------|
| Volcano (`@volcano-sh`)    | `@JesseStutler` | `jesseincomparable@hotmail.com` |
| Your-Scheduler-Here        | `@your-id`      | `your@email.com`                |

We welcome integrations for more custom schedulers that support gang scheduling capabilities. Getting started with:

1.  Open an issue on the [LeaderWorkerSet GitHub repository](https://github.com/kubernetes-sigs/lws/issues) to discuss your proposal, tagging the LWS maintainers.
2.  In the same pull request that adds the scheduler integration, update the maintainer matrix above with your contact information. This can help us contact the maintainer to regularly upgrade the custom scheduler dependencies.

## Using Volcano for Gang Scheduling

### Installation

To enable gang scheduling, you must enable the feature flag and specify `volcano` as the provider during the LeaderWorkerSet controller installation.

- **With Helm:**

  Refer to the [install-by-helm](https://lws.sigs.k8s.io/docs/installation/#install-by-helm). When installing, add the following flags to your `helm install` command:
  ```sh
  helm install lws oci://ghcr.io/kubernetes-sigs/lws-charts/lws \
    --set gangSchedulingManagement.schedulerProvider=volcano
  ```

- **With kubectl:**

  Refer to the [install-by-kubectl](https://lws.sigs.k8s.io/docs/installation/#install-by-kubectl). Gang scheduling is **disabled by default**. To enable gang scheduling capabilities, you need to:

  1. **Update the configuration ConfigMap** to enable gang scheduling settings:
     ```yaml
     apiVersion: v1
     kind: ConfigMap
     metadata:
       name: lws-manager-config
       namespace: lws-system
     data:
       controller_manager_config.yaml: |
         apiVersion: config.lws.x-k8s.io/v1alpha1
         kind: Configuration
         leaderElection:
           leaderElect: true
         internalCertManagement:
           enable: true
         # Add gang scheduling configuration
         gangSchedulingManagement:
           schedulerProvider: volcano
     ```

  2. **Restart the lws-controller-manager** to apply the new configuration:
     ```sh
     kubectl rollout restart deployment/lws-controller-manager -n lws-system
     ```

     Or you can directly delete the pod to trigger a restart:
     ```sh
     kubectl delete pod -l control-plane=controller-manager -n lws-system
     ```

### Startup Policy Differences

The `startupPolicy` in your LeaderWorkerSet spec determines the gang scheduling behavior by setting the `minMember` of the auto-generated `PodGroup`.

- **`LeaderCreated` Policy (Default):** `MinMember` is set to the full replica size (1 leader + (size-1) workers). All pods in the replica must be scheduled together.
- **`LeaderReady` Policy:** `MinMember` is set to 1, therefore the leader pod can be scheduled immediately and won't be blocked by the gang scheduler. 
However, the `minResources` required by the `PodGroup` will still be calculated based on the entire group. 
This ensures that resources for all pods are reserved before the leader starts, guaranteeing that the workers can eventually be scheduled.

### Specifying a Custom Queue

To assign all groups of a LeaderWorkerSet to a specific Volcano queue, add the `scheduling.volcano.sh/queue-name` annotation to the LeaderWorkerSet metadata.


### Volcano Scheduler Configurations

To configure certain annotations in the PodGroup that are used by the Volcano scheduler to control specific scheduling behaviors, the PodGroup created by the LeaderWorkerSet will inherit the annotations starting with `volcano.sh/` from the LeaderWorkerSet's metadata.annotations.

For a complete example, please refer to the [lws-sample-volcano.yaml](./lws-sample-volcano.yaml) file.

