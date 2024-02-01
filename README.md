# droplet-autoscaling

Droplet cluster autoscaling allows a developer to create a cluster of droplets that automatically scales based on load. To use cluster autoscaling, a developer creates a base droplet, takes a snapshot of the droplet and adds the droplet to a load balancer. Clones of the base droplet can be automatically added or removed from the cluster as load changes.

## How to use it

Droplet cluster autoscaling is one file of code called “scale.go” that can run as a scheduled or standalone function. It must be passed configuration parameters including the droplet tag for the cluster, load balancer name, a DigitalOcean API key, etc. and an operation to perform.

The operation to perform (op) must be one of:

* **info** return internal configuration information about the cluster
* **status** return the status of the cluster including load metrics
* **up** scale the cluster up manually by adding a droplet
* **down** scale the cluster down manually by deleting the last droplet added
* **auto** autoscale the cluster up or down based on load metrics

Example:

* <.. function url path..>/scale?op=info
 
## The following environment variables are required:

* **API_TOKEN** a DigitalOcean API token
* **BASE_DROPLET** the name of the base droplet to scale
* **SNAPSHOT** To use droplet autoscaling, you must take a snapshot of your base droplet and that snapshot will be the system image used by all clone droplets. The snapshot should have server(s) that automatically start when the new system boots. The snapshotName is the name of this base droplet snapshot.
* **CLUSTER_TAG** the droplet tag for the cluster. The base droplet must have this tag. All clones that are created will have this tag on them.
* **CLONE_PREFIX** the name of clone droplets is the dropletNamePrefix and the number of the clone. The first clone will be <prefix>01, the second <prefix>02, etc. An example of a droplet name prefix is “auto”
* **LOAD_BALANCER** Droplet autoscaling requires a load balancer. You can find the DigitalOcean load balancer under the Network section of the DigitalOcean user-interface. The base droplet must be added to the load balancer. Clone droplets will automatically be added and removed from the load balancer cluster as the system scales up and down. The loadBalancerName is the name of this load balancer.

 
# Scaling Up and Down
  
A "status" operation can be used to show the status of the cluster. The cluster status shows the metrics for each droplet and the average metrics for the cluster as a whole. A recommendation to scale up or down is also shown. The "auto" operation performs a "status" and also actually scales up or down the cluster based on the calculated recommendation.
  
Metrics are only valid for a droplet in a cluster if metric values can be obtained for the droplet. Metrics are not available for droplets just added to a cluser until the droplet actually reports and stores load metrics so droplet metrics will be market invalid for just-added droplets for a short period of time. Metrics are only valid for a cluster if all droplets in the cluster have valid metrics. A cluster will also be marked as having invalid metrics during the SCALING_INTERVAL, immediately after a clone drop is added or removed, since individual droplet metrics aren't immediately stable after change to the cluster.
  
To determine whether to scale up, metrics for all droplets in the cluster are obtained and averaged. If any of the average cluster metrics for load, free memory or bandwidth are out of range (above the range for load and bandwidth or below the range for free memory), the system will recommend scaling up the cluster. When a cluster has invalid metrics, the system will not recommend scaling up or down and, if given an "auto" command, will not scale up or down.

## The following environment variables are optional.
    
* **MAX_DROPLETS_IN_CLUSTER** Defaults to 5. The maximum number of droplets in the cluster inclusive of the base droplet. The system will not scale up beyond this number of droplets.
* **SCALING_INTERVAL** Defaults to 120 seconds. After a clone droplet is added or removed, the system will stop autoscaling up or down until this interval has passed. The system adds or removes clones automatically based on load metrics. These metrics take some time to stabilize after a clone is added or removed. The scalingInterval is the amount of time it takes the system to stabilize, in seconds, after a droplet is added or removed and should not only allow the system to stabilize but also account for the time it takes the new load metrics from the droplets to be available.
* **SCALE_UP_WHEN_LOAD** When the average CPU load metric is greater than this value, the system will recommend scaling up.
* **SCALE_UP_WHEN_FREE_MEMORY** When the average droplet free memory is below this value, in MB, the system will recommend scaling up.
* **SCALE_UP_WHEN_OUTBOUND_BANDWIDTH** When the average droplet bandwidth, in MBps, is greater than this value, the system will recommend scaling up.
* **SCALE_DOWN_WHEN_LOAD** When the average CPU load metric is less than this value and all other SCALE_DOWN metric values are within range, the system will recommend scaling down.
* **SCALE_DOWN_WHEN_FREE_MEMORY** When the average droplet free memory is above this value and all other SCALE_DOWN metric values are within range, in MB, the system will recommend scaling down.
* **SCALE_DOWN_WHEN_OUTBOUND_BANDWIDTH** When the average droplet bandwidth, in MBps, is less than this value and all other SCALE_DOWN metric values are within range, the system will recommend scaling down.
