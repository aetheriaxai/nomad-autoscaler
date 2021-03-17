package plugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/external"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/hashicorp/nomad-autoscaler/sdk/helper/scaleutils"
	"github.com/hashicorp/nomad/api"
)

const (
	defaultRetryInterval  = 10 * time.Second
	defaultRetryLimit     = 15
	nodeAttrAWSInstanceID = "unique.platform.aws.instance-id"
)

// setupAWSClients takes the passed config mapping and instantiates the
// required AWS service clients.
func (t *TargetPlugin) setupAWSClients(config map[string]string) error {

	// Load our default AWS config. This handles pulling configuration from
	// default profiles and environment variables.
	cfg, err := external.LoadDefaultAWSConfig()
	if err != nil {
		return fmt.Errorf("failed to load default AWS config: %v", err)
	}

	// Check for a configured region and set the value to our internal default
	// if nothing is found.
	region, ok := config[configKeyRegion]
	if !ok {
		region = configValueRegionDefault
	}

	// If the default config is empty, update it.
	if cfg.Region == "" {
		t.logger.Trace("setting AWS region for client", "region", region)
		cfg.Region = region
	}

	// Attempt to pull access credentials for the AWS client from the user
	// supplied configuration. In order to use these static credentials both
	// the access key and secret key need to be present; the session token is
	// optional.
	keyID, idOK := config[configKeyAccessID]
	secretKey, keyOK := config[configKeySecretKey]
	session := config[configKeySessionToken]

	if idOK && keyOK {
		t.logger.Trace("setting AWS access credentials from config map")
		cfg.Credentials = aws.NewStaticCredentialsProvider(keyID, secretKey, session)
	}

	// Set up our AWS client.
	t.asg = autoscaling.New(cfg)

	return nil
}

// scaleOut updates the Auto Scaling Group desired count to match what the
// Autoscaler has deemed required.
func (t *TargetPlugin) scaleOut(ctx context.Context, asg *autoscaling.AutoScalingGroup, count int64) error {

	// Create a logger for this action to pre-populate useful information we
	// would like on all log lines.
	log := t.logger.With("action", "scale_out", "asg_name", *asg.AutoScalingGroupName,
		"desired_count", count)

	input := autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName: asg.AutoScalingGroupName,
		AvailabilityZones:    asg.AvailabilityZones,
		DesiredCapacity:      aws.Int64(count),
	}

	// Ignore the response from Send() as its empty.
	_, err := t.asg.UpdateAutoScalingGroupRequest(&input).Send(ctx)
	if err != nil {
		return fmt.Errorf("failed to update Autoscaling Group: %v", err)
	}

	if err := t.ensureASGInstancesCount(ctx, count, *asg.AutoScalingGroupName); err != nil {
		return fmt.Errorf("failed to confirm scale out AWS AutoScaling Group: %v", err)
	}

	log.Info("successfully performed and verified scaling out")
	return nil
}

func (t *TargetPlugin) scaleIn(ctx context.Context, asg *autoscaling.AutoScalingGroup, num int64, config map[string]string) error {

	// The AWS plugin utilises the individual scaleutils calls due to the way
	// in which the TerminateInstanceInAutoScalingGroupRequest call works. We
	// need to ensure the nodes are part of the target ASG before performing
	// actions, and draining the nodes just to get an error at this phase is
	// wasteful and means we need reconciliation.
	if t.clusterUtils.ClusterNodeIDLookupFunc == nil {
		return errors.New("required ClusterNodeIDLookupFunc not set")
	}

	nodes, err := t.clusterUtils.IdentifyScaleInNodes(config, int(num))
	if err != nil {
		return err
	}

	nodeResourceIDs, err := t.clusterUtils.IdentifyScaleInRemoteIDs(nodes)
	if err != nil {
		return err
	}

	// Any error received here indicates misconfiguration between the ASG and
	// the Nomad node pool.
	instanceIDs, err := instancesBelongToASG(asg, nodeResourceIDs)
	if err != nil {
		return err
	}

	if err := t.clusterUtils.DrainNodes(ctx, config, nodeResourceIDs); err != nil {
		return err
	}
	t.logger.Info("pre scale-in tasks now complete")

	// Create the event writer and write that the drain event has been
	// completed which is part of the RunPreScaleInTasks() function.
	eWriter := newEventWriter(t.logger, t.asg, instanceIDs, *asg.AutoScalingGroupName)
	eWriter.write(ctx, scalingEventDrain)

	// Create a logger for this action to pre-populate useful information we
	// would like on all log lines.
	log := t.logger.With("action", "scale_in", "asg_name", *asg.AutoScalingGroupName)

	// Run the termination and log the results.
	result := t.terminateInstancesInASG(ctx, nodeResourceIDs)
	result.logResults(log)

	// If we have any failures, perform our revert so we don't leave nodes in
	// an undesired state.
	if result.lenFailure() > 0 {
		t.clusterUtils.RunPostScaleInTasksOnFailure(result.failedIDs())
	}

	// If we had successfully termination from the ASG, track these activities
	// until completion. A failure here should not fail the scaling activity as
	// AWS should honour the contract, it could be a case of there being
	// slowness in the AWS system and us timing out.
	if result.lenSuccess() > 0 {

		t.logger.Debug("ensuring AWS ASG activities complete")

		if err := t.ensureActivitiesComplete(ctx, *asg.AutoScalingGroupName, result.activityIDs()); err != nil {
			log.Error("failed to ensure all activities completed", "error", err)
		} else {
			t.logger.Debug("confirmed AWS ASG activities completed")
		}
		eWriter.write(ctx, scalingEventTerminate)

		// Run any post scale in tasks that are desired.
		if err := t.clusterUtils.RunPostScaleInTasks(ctx, config, result.successfulIDs()); err != nil {
			return fmt.Errorf("failed to perform post-scale Nomad scale in tasks: %v", err)
		}
	}

	if result.lenFailure() > 0 && result.lenSuccess() > 0 {
		log.Warn("partial scaling success",
			"success_num", result.lenSuccess(), "failed_num", result.lenFailure())
		return nil
	}
	return result.errorOrNil()
}

// instancesBelongToASG checks that all the instances identified for scaling in
// belong to the target ASG.
func instancesBelongToASG(asg *autoscaling.AutoScalingGroup, ids []scaleutils.NodeResourceID) ([]string, error) {

	// Grab the instanceIDs once as it is used multiple times throughout the
	// scale in event.
	var instanceIDs []string

	// isMissing tracks the total number of instance deemed missing from the
	// ASG to provide some user context.
	var isMissing int

	for _, node := range ids {

		// found identifies whether this individual node has been located
		// within the ASG.
		var found bool

		// Iterate the instance within the ASG, and exit if we identify a
		// match to continue below.
		for _, asgIDs := range asg.Instances {
			if node.RemoteResourceID == *asgIDs.InstanceId {
				found = true
				break
			}
		}

		if found {
			instanceIDs = append(instanceIDs, node.RemoteResourceID)
		} else {
			isMissing++
		}
	}

	if isMissing > 0 {
		return nil, fmt.Errorf("%v selected nodes are not found within ASG", isMissing)
	}
	return instanceIDs, nil
}

// terminateInstancesInASG handles terminating all instances passed and returns
// an object detailing the complete status of the performed action.
func (t *TargetPlugin) terminateInstancesInASG(ctx context.Context, ids []scaleutils.NodeResourceID) instanceTerminationResult {

	var status instanceTerminationResult

	for _, id := range ids {
		activityID, err := t.terminateInstance(ctx, id.RemoteResourceID)
		if err != nil {
			status.appendFailure(instanceFailure{instance: id, err: err})
			continue
		}
		status.appendSuccess(instanceSuccess{instance: id, activityID: activityID})
	}

	return status
}

// terminateInstancesInASG terminates a single instance within an AWS
// AutoScaling Group. It returns any error from the API, along with the
// activity ID from the scaling event.
func (t *TargetPlugin) terminateInstance(ctx context.Context, id string) (*string, error) {

	asgInput := autoscaling.TerminateInstanceInAutoScalingGroupInput{
		InstanceId:                     aws.String(id),
		ShouldDecrementDesiredCapacity: aws.Bool(true),
	}

	// The underlying AWS client HTTP request includes backoff and retry in the
	// event of errors such as timeouts and rate-limiting. There is therefore
	// no value in retrying requests that fail.
	resp, err := t.asg.TerminateInstanceInAutoScalingGroupRequest(&asgInput).Send(ctx)
	if err != nil {
		return nil, err
	}

	// It's unknown whether this will ever hit in the event the return error is
	// nil, but we should protect against a nil pointer error. The ActivityId
	// is required, therefore if Activity is not nil, this should be there.
	if resp.Activity == nil {
		return nil, errors.New("AWS returned nil activity response object")
	}
	return resp.Activity.ActivityId, nil
}

func (t *TargetPlugin) describeASG(ctx context.Context, asgName string) (*autoscaling.AutoScalingGroup, error) {

	input := autoscaling.DescribeAutoScalingGroupsInput{AutoScalingGroupNames: []string{asgName}}

	resp, err := t.asg.DescribeAutoScalingGroupsRequest(&input).Send(ctx)
	if err != nil {
		return nil, err
	}

	if len(resp.AutoScalingGroups) != 1 {
		return nil, fmt.Errorf("expected 1 Autoscaling Group, got %v", len(resp.AutoScalingGroups))
	}
	return &resp.AutoScalingGroups[0], nil
}

func (t *TargetPlugin) describeActivities(ctx context.Context, asgName string, ids []string) ([]autoscaling.Activity, error) {

	input := autoscaling.DescribeScalingActivitiesInput{AutoScalingGroupName: aws.String(asgName)}

	// If an ID is specified, add this to the request so we only pull
	// information regarding this.
	if len(ids) > 0 {
		input.ActivityIds = ids
	}

	resp, err := t.asg.DescribeScalingActivitiesRequest(&input).Send(ctx)
	if err != nil {
		return nil, err
	}

	// If the caller passed a list of IDs to describe, ensure the returned list
	// is the current length.
	if len(ids) > 0 && len(resp.Activities) != len(ids) {
		return nil, fmt.Errorf("expected %v activities, got %v", len(ids), len(resp.Activities))
	}
	return resp.Activities, nil
}

func (t *TargetPlugin) ensureActivitiesComplete(ctx context.Context, asg string, ids []string) error {

	f := func(ctx context.Context) (bool, error) {

		activities, err := t.describeActivities(ctx, asg, ids)
		if err != nil {
			return true, err
		}

		// Reset the scaling activity IDs we are waiting to complete so we can
		// re-populate with a modified list later.
		ids = []string{}

		// Iterate each activity, check the progress and add any incomplete
		// activities to the ID list for rechecking.
		for _, activity := range activities {
			if *activity.Progress != 100 {
				ids = append(ids, *activity.ActivityId)
			}
		}

		// If we dont have any remaining IDs to check, we can finish.
		if len(ids) == 0 {
			return true, nil
		}
		return false, fmt.Errorf("waiting for %v activities to finish", len(ids))
	}

	return retry(ctx, defaultRetryInterval, defaultRetryLimit, f)
}

func (t *TargetPlugin) ensureASGInstancesCount(ctx context.Context, desired int64, asgName string) error {

	f := func(ctx context.Context) (bool, error) {
		asg, err := t.describeASG(ctx, asgName)
		if err != nil {
			return true, err
		}

		if len(asg.Instances) == int(desired) {
			return true, nil
		}
		return false, fmt.Errorf("AutoScaling Group at %v instances of desired %v", asg.Instances, desired)
	}

	return retry(ctx, defaultRetryInterval, defaultRetryLimit, f)
}

// awsNodeIDMap is used to identify the AWS InstanceID of a Nomad node using
// the relevant attribute value.
func awsNodeIDMap(n *api.Node) (string, error) {
	val, ok := n.Attributes[nodeAttrAWSInstanceID]
	if !ok || val == "" {
		return "", fmt.Errorf("attribute %q not found", nodeAttrAWSInstanceID)
	}
	return val, nil
}
