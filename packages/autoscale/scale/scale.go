package main

import (
  "encoding/json"
  "regexp"
  "io"
  "bytes"
  "errors"
  "strings"
  "strconv"
  "context"
  "fmt"
  "time"
  "os"
  "github.com/digitalocean/godo"
  "github.com/digitalocean/godo/metrics"
)

// DigitalOcean API token

var apiToken string

// User configuration with default values

var conf = ClusterConfig {
  MaxDropletsInCluster: 5,
  ScalingInterval: 180,

  // NOTE: ANY average cluster value over/under a limit will cause a cluster to scale up
  //       load is a high limit, freeMemory is a low limit, outboundBandwidth is a high limit
  //       a value of -1.0 causes that value to be ignored

  ScaleUpWhen: Metrics {
    Valid: true,
    Load: 0.35,
    FreeMemory: 800.0,
    OutboundBandwidth: 2.5,
  },

  // NOTE: ALL average cluster values must be over/under their limits for the cluster to scale down
  //       load is a maximum, freeMemory is a minimum, outboundBandwidth is a maximum

  ScaleDownWhen : Metrics {
    Valid: true,
    Load: 0.15,
    FreeMemory: 900.0,
    OutboundBandwidth: 1.5,
  },
}

type ClusterConfig struct {
  Tag string `json:"tag"`
  BaseDropletName string `json:"baseDropletName"`
  SnapshotName string `json:"snapshotName"`
  CloneNamePrefix string `json:"cloneNamePrefix"`
  LoadBalancerName string `json:"LoadBalancerName"`
  MaxDropletsInCluster int `json:"maxDropletsInCluster"`
  ScalingInterval int `json:"scalingInterval"`
  ScaleUpWhen Metrics `json:"scaleUpWhen"`
  ScaleDownWhen Metrics `json:"scaleDownWhen"`
}

type ClusterInfo struct {
  DropletCount int `json:"dropletCount"`
  Droplets []godo.Droplet `json:"droplets"`
  BaseDroplet *godo.Droplet `json:"baseDroplet"`
  Region string `json:"region"`
  DropletSize string `json:"dropletSize"`
  SnapshotImageID int `json:"snapshotImageID"`
  LoadBalancerID string `json:"loadBalancerID"`
}

type Metrics struct {
  ID string `json:"ID"`
  Valid bool `json:"valid"`
  Load float64 `json:"load"`
  FreeMemory float64 `json:"freeMemory"`
  OutboundBandwidth float64 `json:"outboundBandwidth"`
}

type ClusterMetrics struct {
  droplets []Metrics
  metrics Metrics
}

// Returns a ClusterInfo struct that gives information about a given droplet cluster

func getClusterInfo(client *godo.Client, ctx context.Context, conf ClusterConfig) (info ClusterInfo, err error) {
  listOptions := &godo.ListOptions{
    Page: 1,
    PerPage: 50,
  }

  for listOptions.Page = 1; true; listOptions.Page++ { 
    droplets, resp, e := client.Droplets.ListByTag(ctx, conf.Tag, listOptions)
    if e != nil {
      err = fmt.Errorf("couldn't list cluster: %s\n", e)
      return
    }
    info.Droplets = append(info.Droplets, droplets...)
    info.DropletCount += len(droplets)
    if (resp.Links == nil || resp.Links.IsLastPage()) {
      break
    }
  }

  // NOTE: we can copy the region/size/slug names from any droplet in the
  // cluster since they are all the same. We take the first one here.

  if (info.DropletCount > 0) {
    info.Region =  info.Droplets[0].Region.Slug
    info.DropletSize = info.Droplets[0].Size.Slug
  }

  for listOptions.Page = 1; true; listOptions.Page++ { 
    images, resp, _ := client.Images.ListUser(ctx, listOptions)
    info.SnapshotImageID = -1
    for _, image := range images {
      if (image.Name == conf.SnapshotName) {
        info.SnapshotImageID = image.ID
      }
    }
    if (resp.Links == nil || resp.Links.IsLastPage()) {
      break
    }
  }

  for listOptions.Page = 1; true; listOptions.Page++ { 
    lbs, resp, _ := client.LoadBalancers.List(ctx, listOptions)
    info.LoadBalancerID = ""
    for _, lb := range lbs {
      if (lb.Name == conf.LoadBalancerName) {
        info.LoadBalancerID = lb.ID
      }
    }
    if (resp.Links == nil || resp.Links.IsLastPage()) {
      break
    }
  }

  info.BaseDroplet = findDropletByName(conf.BaseDropletName, info)
  if (info.BaseDroplet == nil) {
      err = fmt.Errorf("can't find base droplet named %s\n", conf.BaseDropletName)
      return
  }

  return
}

// Returns the droplet in the cluster with the given name

func findDropletByName(name string, info ClusterInfo) *godo.Droplet {
  for _, droplet := range info.Droplets {
    if (droplet.Name == name) {
      return &droplet
    }
  }
  return nil
}

// Returns the scaling tag for the cluster with the given tag prefix

func getScalingTag(info ClusterInfo) (tagPrefix string, existingTag string) {
  tagPrefix = "scaling-" + conf.CloneNamePrefix + "-"

  for _, tag := range info.BaseDroplet.Tags {
    if (strings.HasPrefix(tag, tagPrefix)) {
      existingTag = tag
      return
    }
  }
  return
}

// Returns true if the cluster is scaling. That is, if a scaling operations has
// been performed (add/delete clone droplet) and the scalingInterval since that
// operation has not expired

func isClusterScaling(client *godo.Client, ctx context.Context, conf ClusterConfig, info ClusterInfo) bool {

  // Find the scaling tag, if it exists. If no scaling tag exists, we aren't scaling

  _, tag := getScalingTag(info)
  if (tag == "") {
    return false
  }
  re := regexp.MustCompile(`(\w+)-(\w+)-(\d+)`)
  parts := re.FindStringSubmatch(tag)
  if parts == nil || len(parts) != 4 {
    return false
  }

  // If time shown in the scaling tag has expired, delete the scaling tag and return false

  t, _ := strconv.ParseInt(parts[3], 10, 64)
  now := time.Now().Unix()
  if (now - t > int64(conf.ScalingInterval)) {
    // If the delete fails, we'll try again later if we encounter this tag again
    client.Tags.Delete(ctx, tag)
    return false
  }

  return true
}

// Sets a tag on the base droplet to mark that a scaling operation just occured

func markClusterScaling(client *godo.Client, ctx context.Context, conf ClusterConfig, info ClusterInfo) error {

  // Delete any existing scaling tag

  tagPrefix, tag := getScalingTag(info)
  if (tag != "") {
    _, err := client.Tags.Delete(ctx, tag)
    if (err != nil) {
      return errors.New("unable to delete existing tag: " + err.Error())
    }
  }

  // Add a new (or replaces existing) scaling tag to the base droplet

  tag = tagPrefix + strconv.FormatInt(time.Now().Unix(), 10)

  tagCreateRequest := &godo.TagCreateRequest { Name: tag }
  _, _, err := client.Tags.Create(ctx, tagCreateRequest)
  if (err != nil) {
    return errors.New("unable to create tag: " + err.Error())
  }

  tagResourcesRequest := &godo.TagResourcesRequest {
    []godo.Resource { godo.Resource { ID: strconv.Itoa(info.BaseDroplet.ID), Type: godo.DropletResourceType } },
  }
  _, err = client.Tags.TagResources(ctx, tag, tagResourcesRequest)
  if (err != nil) {
    return errors.New("unable to tag resource: " + err.Error())
  }

  return nil
}

// Creates a new clone droplet in the cluster, with image loaded from the snapshot in the config

func addDroplet(client *godo.Client, ctx context.Context, conf ClusterConfig, info ClusterInfo) (dropletID int, err error) {
    dropletName := fmt.Sprintf("%s%03d", conf.CloneNamePrefix, info.DropletCount)
    
    // NOTE: new droplet is namedf: fmt.Printf(output, "%s\n", dropletName)

    createRequest := &godo.DropletCreateRequest{
      Name: dropletName,
      Region: info.Region,
      Tags: []string{conf.Tag},
      Size: info.DropletSize,
      Image: godo.DropletCreateImage{ ID: info.SnapshotImageID },
    }
    dropletID = -1
    newDroplet, _, e := client.Droplets.Create(ctx, createRequest)
    if e != nil {
      err = fmt.Errorf("couldn't create droplet %s\n", e)
      return
    }
    dropletID = newDroplet.ID
    return
}

// Waits until the given droplet has an IPv4 address, signifying its availabilty to be used
//
// NOTE: There is also a droplet.WaitForActive in godo itself (util/droplet.go) but we use our own here

func waitForDropletUp(client *godo.Client, ctx context.Context, dropletID int) error {
  var newDroplet *godo.Droplet
  var err error
  for i := 1; i < 100; i++ {
    time.Sleep(2 * time.Second)

    newDroplet, _, err = client.Droplets.Get(ctx, dropletID)
    if err != nil {
      return errors.New("waiting error droplet get")
    }
    if (newDroplet == nil) {
      continue;
    }
    ipv4, err := newDroplet.PublicIPv4()
    if err != nil {
      return errors.New("waiting error getting ipv4")
    }
    if (ipv4 != "") {
      // NOTE: IPv4 address is: fmt.Printf(output, "%s\n", ipv4)
      break
    }
  }
  if (newDroplet == nil) {
    return errors.New("timeout")
  }
  // NOTE: the droplet is fmt.Printf(output, "%s %s\n", newDroplet.String(), newDroplet.URN())
  return nil
}

// Deletes the last (most recently added) droplet in an autoscaling cluster

func deleteLastDroplet(client *godo.Client, ctx context.Context, conf ClusterConfig, info ClusterInfo) error {
  name := fmt.Sprintf("%s%03d", conf.CloneNamePrefix, info.DropletCount - 1)

  droplet := findDropletByName(name, info)
  if (droplet == nil) {
      return errors.New("Can't find last droplet ID")
  }
  dropletID := droplet.ID

   _, err := client.Droplets.Delete(ctx, dropletID)

  return err
}

// Adds a droplet to load balancer ID in the given cluster info

func addDropletToLoadBalancer(client *godo.Client, ctx context.Context, info ClusterInfo, dropletID int) error {
  _, err := client.LoadBalancers.AddDroplets(ctx, info.LoadBalancerID, dropletID)
  return err
}

// NOTE: to help understand the DigitalOcean metrics structures I've included
// them in this comment for reference:
//
// type LabelValue string
// type LabelName string
// type LabelSet map[LabelName]LabelValue
// type Metric LabelSet
// type SampleValue float64
//
// type SamplePair struct {
//   Timestamp Time
//   Value SampleValue
// }
// type SampleStream struct {
//  Metric Metric       `json:"metric"`
//  Values []SamplePair `json:"values"`
// }
//
// and m.Data.Result is a []metrics.SampleStream

// Calculates the average value from an array of SamplePair values

func calcAverageValue(values []metrics.SamplePair) float64 {
  x := 0.0
  for _, p := range values {
    x += float64(p.Value)
  }
  return x / float64(len(values))
}

// Return the various load metrics (load, free memory, bandwidth) for the droplets in the cluster and also the
// average metrics for the cluster itself. Droplet metrics are only marked as valid if all metrics for the droplet are available.
// Cluster metrics are only marked valid if all metrics in the cluster are valid

func getClusterMetrics(client *godo.Client, ctx context.Context, conf ClusterConfig, info ClusterInfo) (cmetrics ClusterMetrics, err error) {
    now := time.Now()
    numDroplets := len(info.Droplets)
    if (numDroplets < 1) {
        return cmetrics, errors.New("no droplets")
    }
    cmetrics.droplets = make([]Metrics, numDroplets)

    var numValidDropletMetrics int
    for i, droplet := range info.Droplets {
      cmetrics.droplets[i].ID = droplet.Name

      metricsRequest := &godo.DropletMetricsRequest{
        HostID: strconv.Itoa(droplet.ID),
        Start: now.Add(-time.Minute * 4),
        End: now,
      }

      // Calculate load metric for droplet

      m, _, err := client.Monitoring.GetDropletLoad1(ctx, metricsRequest)
      if err != nil {
        continue
      }
      if len(m.Data.Result) != 1 || len(m.Data.Result[0].Values) < 1 {
        continue
      }

      x := calcAverageValue(m.Data.Result[0].Values)
      cmetrics.droplets[i].Load = x
      cmetrics.metrics.Load += x

      // Calculate free memory for droplet

      m, _, err = client.Monitoring.GetDropletFreeMemory(ctx, metricsRequest)
      if err != nil {
        continue
      }
      if len(m.Data.Result) != 1 || len(m.Data.Result[0].Values) < 1 {
        continue
      }

      // NOTE: we divide by 1000000 to turn free memory into MB
      x = calcAverageValue(m.Data.Result[0].Values) / 1000000.0
      cmetrics.droplets[i].FreeMemory = x
      cmetrics.metrics.FreeMemory += x

      // Calculate outboundBandwidth for droplet
      //
      // NOTE: we do both public and private interface so we can see outbound bandwidth
      // from one DigitalOcean droplet to another

      var interfaces = []string{"public", "private"}
      for _, interfaceType := range interfaces {
        bandwidthRequest := &godo.DropletBandwidthMetricsRequest{
          DropletMetricsRequest: *metricsRequest,
          Interface: interfaceType,
          Direction: "outbound",
        }
        m, _, err = client.Monitoring.GetDropletBandwidth(ctx, bandwidthRequest)
        if err != nil {
          continue
        }

        if len(m.Data.Result) != 1 || len(m.Data.Result[0].Values) < 1 {
          // NOTE: missing bandwidth values is not an error because when a machine
          // comes up, it doesn't register bandwidth immediately so we have
          // a zero value to no data here
          x = 0.0
        } else {
          x = calcAverageValue(m.Data.Result[0].Values)
        }
        cmetrics.droplets[i].OutboundBandwidth += x
        cmetrics.metrics.OutboundBandwidth += x
      }

      // The metrics for a droplet are valid Only If we have all the metircs for a droplet

      numValidDropletMetrics++
      cmetrics.droplets[i].Valid = true
    }

    // The clusters metrics aren't valid if the cluster is in the process of scaling. That is,
    // if we add or remove a clone then we need to wait "scalingInterval" seconds until the
    // cluster is stable. During the time it is scaling, the metrics won't be valid because they
    // are old since the scaling hasn't taken effect (load hasn't shifted and metrics haven't
    // reflected the added or removed clone)
    //
    // The cluster itself also only has valid metrics if all the droplets have valid metrics. A droplet won't
    // have valid metrics if there are some missing. This can happen when a droplet is added to the
    // cluster and it hasn't logged metrics yet. We don't want to do any scaling operations if any
    // of the droplets have invalid metrics since we don't know the actual status of the cluster


    if isClusterScaling(client, ctx, conf, info) == false && numValidDropletMetrics == len(info.Droplets) {
      cmetrics.metrics.Valid = true
    }
    cmetrics.metrics.Load /= float64(numDroplets)
    cmetrics.metrics.FreeMemory /= float64(numDroplets)
    cmetrics.metrics.OutboundBandwidth /= float64(numDroplets)

    return cmetrics, nil
}

func getClusterStatus(client *godo.Client, ctx context.Context, conf ClusterConfig, info ClusterInfo) (resp StatusResponse, err error) {

  cm, e := getClusterMetrics(client, ctx, conf, info)
  if e != nil {
    err = errors.New("can't get cluster metrics: " + err.Error())
    return
  }

  resp = StatusResponse { cm.droplets, cm.metrics, "", false, false }

  // Determine if we should scale up or down

  if (cm.metrics.Valid == false) {
      resp.recommendation = "Recommendation: no change (cluster metrics are not valid, cluster may be in the process of scaling)"
  } else if (cm.metrics.Load > conf.ScaleUpWhen.Load || cm.metrics.FreeMemory < conf.ScaleUpWhen.FreeMemory ||
      cm.metrics.OutboundBandwidth > conf.ScaleUpWhen.OutboundBandwidth) {
    if (info.DropletCount >= conf.MaxDropletsInCluster) {
      resp.recommendation = "no change (can't scale up, reached max droplet count)"
    } else {
      resp.recommendation = "scale up"
      resp.ScaleUp = true
    }
  } else if (cm.metrics.Load < conf.ScaleDownWhen.Load && cm.metrics.FreeMemory > conf.ScaleDownWhen.FreeMemory &&
      cm.metrics.OutboundBandwidth < conf.ScaleDownWhen.OutboundBandwidth) {
    if (info.DropletCount == 1) {
      resp.recommendation = "no change (can't scale down, only 1 droplet in cluster)"
    } else {
      resp.recommendation = "scale down"
      resp.ScaleDown = true
    }
  } else {
      resp.recommendation = "no change"
  }
  return
}

type ErrorResponse struct {
  Err string `json:"error"`
}

func outputError(w io.Writer, s string) {
  j, _ := json.Marshal(ErrorResponse { s })
  fmt.Fprintln(w, string(j))
}

type InfoResponse struct {
  Config ClusterConfig `json:"config"`
  Info ClusterInfo `json:"info"`
}

type StatusResponse struct {
  DropletMetrics []Metrics `json:"dropletMetrics"`
  ClusterMetrics Metrics `json:"clusterMetrics"`
  recommendation string `json:"recommendation"`
  ScaleUp bool `json:"scaleUp"`
  ScaleDown bool  `json:"scaleDown"`
}

type ScaleResponse struct {
  Success bool `json:"success"`
  Msg string `json:"msg"`
}

type AutoResponse struct {
   Status StatusResponse `json:"status"`
   ScaleUp ScaleResponse `json:"scaleUp"`
   ScaleDown ScaleResponse `json:"scaleDown"`
}

// The main function that performs the given operation op where op is one of [info|up|down|status|auto]

func funcMain(op string, output io.Writer) {

  client := godo.NewFromToken(apiToken)
  ctx := context.TODO()

  info, err := getClusterInfo(client, ctx, conf)

  if (err != nil) {
    outputError(output, "can't get cluster info: " + err.Error())
    return
  }
  if (info.DropletCount == 0) {
    outputError(output, "no droplets in cluster with tag " + conf.Tag)
    return
  }
  if (info.SnapshotImageID == -1) {
    outputError(output, "no snapshot with name " + conf.SnapshotName)
    return
  }
  if (info.LoadBalancerID == "") {
    outputError(output, "no load balancer with name " + conf.LoadBalancerName)
    return
  }

  var autoResp AutoResponse
  var scaleUp, scaleDown bool

  switch op {

  case "info":
    j, _ := json.MarshalIndent(InfoResponse { conf, info }, "", " ")
    fmt.Fprintln(output, string(j))

  case "up":
    if (info.DropletCount >= conf.MaxDropletsInCluster) {
      outputError(output, "cluster is at maximum size")
      return
    }
    scaleUp = true

  case "down":
    if (info.DropletCount == 1) {
      outputError(output, "cluster has no autoscaled droplets")
      return
    }
    scaleDown = true

  case "status", "auto":
    resp, err := getClusterStatus(client, ctx, conf, info)

    if (err != nil) {
      outputError(output, "getting cluster status: " + err.Error())
      return
    }

    if (op == "status") {
      j, _ := json.MarshalIndent(resp, "", " ")
      fmt.Fprintln(output, string(j))
    } else {
      autoResp.Status = resp
      scaleUp = resp.ScaleUp
      scaleDown = resp.ScaleDown
    }

  default:
    outputError(output, "unknown or missing op parameter (i.e. /scale?op=[info|up|down|status|auto])")
    return
  }

  // Actually perform the scale up if required

  if (scaleUp) {
    resp := ScaleResponse {}
    var dropletUp bool

    // NOTE: marking the cluster at the start can help prevent failing autoscaling operations from looping quickly

    err = markClusterScaling(client, ctx, conf, info)
    if (err != nil) {
       resp.Msg = "couldn't mark cluster: " + err.Error()
    } else {
      dropletID, err := addDroplet(client, ctx, conf, info)
      if err != nil {
        resp.Msg = "can't create droplet: " + err.Error()
      } else if (dropletID == -1) {
        resp.Msg = "couldn't create a new droplet"
      } else {
        err = waitForDropletUp(client, ctx, dropletID)
        if err != nil {
          resp.Msg = "droplet couldn't initialize: " + err.Error()
        } else {
          dropletUp = true
        }
      }

      if (dropletUp) {
        err = addDropletToLoadBalancer(client, ctx, info, dropletID)

        if err != nil {
          resp.Msg = "could not add droplet to load balancer: " + err.Error()
        } else {
          resp.Success = true
          resp.Msg = "droplet created and added to load balancer"
        }
      }
    }

    if (op == "up") {
      j, _ := json.MarshalIndent(resp, "", " ")
      fmt.Fprintln(output, string(j))
    } else {
      autoResp.ScaleUp = resp
    }
  }

  // Actually perform the scale down if required

  if (scaleDown) {
    resp := ScaleResponse {}

    err = markClusterScaling(client, ctx, conf, info)
    if (err != nil) {
       resp.Msg = "couldn't mark cluster: " + err.Error()
    } else {
      err := deleteLastDroplet(client, ctx, conf, info)
      if err != nil {
        resp.Msg = "couldn't delete last droplet: " + err.Error()
      } else {
        resp.Msg = "clone droplet deleted"
        resp.Success = true
      }
    }

    if (op == "down") {
      j, _ := json.MarshalIndent(resp, "", " ")
      fmt.Fprintln(output, string(j))
    } else {
      autoResp.ScaleDown = resp
    }
  }

  if (op == "auto") {
    j, _ := json.MarshalIndent(autoResp, "", " ")
    fmt.Fprintln(output, string(j))
  }
}

// Loads the required configuration variables from the environment. Returns
// true if there were any errors and false otherwise

func loadConfigRequiredEnv(w io.Writer, conf *ClusterConfig) bool {
  apiToken = os.Getenv("API_TOKEN")
  if (apiToken == "") {
    outputError(w, "an API_TOKEN environment variable is required")
    return true
  }

  conf.Tag = os.Getenv("CLUSTER_TAG")
  if (conf.Tag == "") {
    outputError(w, "a TAG environment variable is required")
    return true
  }

  conf.BaseDropletName = os.Getenv("BASE_DROPLET")
  if (conf.BaseDropletName == "") {
    outputError(w, "an BASE_DROPLET environment variable is required")
    return true
  }

  conf.SnapshotName = os.Getenv("SNAPSHOT")
  if (conf.SnapshotName == "") {
    outputError(w, "a SNAPSHOT_NAME environment variable is required")
    return true
  }

  conf.CloneNamePrefix = os.Getenv("CLONE_PREFIX")
  if (conf.CloneNamePrefix == "") {
    outputError(w, "a DROPLET_NAME_PREFIX environment variable is required")
    return true
  }
  if (strings.Index(conf.CloneNamePrefix, "-") != -1) {
    outputError(w, "clone name prefix must not contain a - character")
    return true
  }

  conf.LoadBalancerName = os.Getenv("LOAD_BALANCER")
  if (conf.LoadBalancerName == "") {
    outputError(w, "a LOAD_BALANCER_NAME environment variable is required")
    return true
  }

  return false
}

// Loads the optional configuration variables from the environment. Returns
// true if there were any errors and false otherwise

func loadConfigOptionalEnv(w io.Writer, conf *ClusterConfig) bool {
  var i int

  s := os.Getenv("MAX_DROPLETS_IN_CLUSTER")
  if (s != "") {
    i, _ = strconv.Atoi(s)
    if (i < 2 || i > 1000) {
      outputError(w, "MAX_DROPLETS_IN_CLUSTER is invalid")
      return true
    }
    conf.MaxDropletsInCluster = i
  }

  s = os.Getenv("SCALING_INTERVAL")
  if (s != "") {
    i, _ = strconv.Atoi(s)
    if (i < 30 || i > 2400) {
      outputError(w, "SCALING_INTERVAL is invalid")
      return true
    }
    conf.ScalingInterval = i
  }

  var f float64
  var err error

  s = os.Getenv("SCALE_UP_WHEN_LOAD")
  if (s != "") {
    f, err = strconv.ParseFloat(s, 32)
    if (err != nil || f < .01 || f > 200.0) {
      outputError(w, "SCALE_UP_WHEN_LOAD is invalid")
      return true
    }
    conf.ScaleUpWhen.Load = f
  }

  s = os.Getenv("SCALE_UP_WHEN_FREE_MEMORY")
  if (s != "") {
    f, err = strconv.ParseFloat(s, 32)
    if (err != nil || f < .01 || f > 50000.0) {
      outputError(w, "SCALE_UP_WHEN_FREE_MEMORY is invalid")
      return true
    }
    conf.ScaleUpWhen.FreeMemory = f
  }

  s = os.Getenv("SCALE_UP_WHEN_OUTBOUND_BANDWIDTH")
  if (s != "") {
    f, err = strconv.ParseFloat(s, 32)
    if (err != nil || f < .01 || f > 100000.0) {
      outputError(w, "SCALE_UP_WHEN_OUTBOUND_BANDWIDTH is invalid")
      return true
    }
    conf.ScaleUpWhen.OutboundBandwidth = f
  }

  s = os.Getenv("SCALE_DOWN_WHEN_LOAD")
  if (s != "") {
    f, err = strconv.ParseFloat(s, 32)
    if (err != nil || f < .01 || f > 200.0) {
      outputError(w, "SCALE_DOWN_WHEN_LOAD is invalid")
      return true
    }
    conf.ScaleDownWhen.Load = f
  }

  s = os.Getenv("SCALE_DOWN_WHEN_FREE_MEMORY")
  if (s != "") {
    f, err = strconv.ParseFloat(s, 32)
    if (err != nil || f < .01 || f > 50000.0) {
      outputError(w, "SCALE_DOWN_WHEN_FREE_MEMORY is invalid")
      return true
    }
    conf.ScaleDownWhen.FreeMemory = f
  }

  s = os.Getenv("SCALE_DOWN_WHEN_OUTBOUND_BANDWIDTH")
  if (s != "") {
    f, err = strconv.ParseFloat(s, 32)
    if (err != nil || f < 0.01 || f > 100000.0) {
      outputError(w, "SCALE_DOWN_WHEN_OUTBOUND_BANDWIDTH is invalid")
      return true
    }
    conf.ScaleDownWhen.OutboundBandwidth = f
  }

  return false
}

type Request struct {
  Op string `json:"op"`
}

// sample main() for invoking via command line, unused when run as a function

func unused_main() {
  if (len(os.Args) != 2) {
    fmt.Printf("Usage: scale [info|up|down|status|auto]\n")
    return
  }
  r, _ := Main(Request { Op: os.Args[1] })
  fmt.Println(r.Body)
}

// Main() when invoked as a DigitalOcean function

type Response struct {
  StatusCode int `json:"statusCode,omitempty"`
  Headers map[string]string `json:"headers,omitempty"`
  Body string `json:"body,omitempty"`
}

func Main(in Request) (*Response, error) {
  var output bytes.Buffer
  var hasError bool

  // load the required and optional environment variables

  hasError = loadConfigRequiredEnv(&output, &conf)
  if !hasError {
    hasError = loadConfigOptionalEnv(&output, &conf)
  }

  if !hasError {
    funcMain(in.Op, &output)
  }

  return &Response {
    Headers: map[string]string { "Content-Type": "application/json" },
    Body: output.String(),
  }, nil
}

