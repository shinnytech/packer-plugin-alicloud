// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package ecs

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"

	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/requests"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/responses"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/ecs"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/vpc"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	confighelper "github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/uuid"
)

type stepCreateAlicloudInstance struct {
	IOOptimized                 confighelper.Trilean
	InstanceType                string
	UserData                    string
	UserDataFile                string
	RamRoleName                 string
	Tags                        map[string]string
	RegionId                    string
	InternetChargeType          string
	InternetMaxBandwidthOut     int
	InstanceName                string
	SecurityEnhancementStrategy string
	AlicloudImageFamily         string
	createdInstanceId           string
}

var createInstanceRetryErrors = []string{
	"IdempotentProcessing",
}

var deleteInstanceRetryErrors = []string{
	"IncorrectInstanceStatus.Initializing",
}

func (s *stepCreateAlicloudInstance) Run(_ context.Context, state multistep.StateBag) multistep.StepAction {
	client := state.Get("client").(*ClientWrapper)
	ui := state.Get("ui").(packersdk.Ui)

	ui.Say("Creating instance...")
	vSwitches := state.Get("vswitches").([]vpc.VSwitch)
	for _, vSwitch := range vSwitches {
		ui.Say(fmt.Sprintf("Try to create instance in zone: %s ...", vSwitch.ZoneId))
		createInstanceRequest, err := s.buildCreateInstanceRequest(state, vSwitch)
		if err != nil {
			return halt(state, err, "")
		}

		createInstanceResponse, err := client.WaitForExpected(&WaitForExpectArgs{
			RequestFunc: func() (responses.AcsResponse, error) {
				return client.CreateInstance(createInstanceRequest)
			},
			EvalFunc: client.EvalCouldRetryResponse(createInstanceRetryErrors, EvalRetryErrorType),
		})

		if err != nil {
			// halt会记录error，影响最终执行status code。这里只需要提示
			ui.Say(fmt.Sprintf("Error creating instance: %s", err))
			continue
		}

		s.createdInstanceId = createInstanceResponse.(*ecs.CreateInstanceResponse).InstanceId

		_, err = client.WaitForInstanceStatus(s.RegionId, s.createdInstanceId, InstanceStatusStopped)
		if err != nil {
			return halt(state, fmt.Errorf("zone: %s \n err: %v", vSwitch.ZoneId, err), "Error waiting created instance")
		}

		describeInstancesRequest := ecs.CreateDescribeInstancesRequest()
		describeInstancesRequest.InstanceIds = fmt.Sprintf("[\"%s\"]", s.createdInstanceId)
		instances, err := client.DescribeInstances(describeInstancesRequest)
		if err != nil {
			return halt(state, err, "")
		}

		ui.Message(fmt.Sprintf("Created instance: %s", s.createdInstanceId))
		instance := &instances.Instances.Instance[0]
		state.Put("instance", instance)
		// instance_id is the generic term used so that users can have access to the
		// instance id inside of the provisioners, used in step_provision.
		state.Put("instance_id", s.createdInstanceId)

		return multistep.ActionContinue
	}
	return halt(state, fmt.Errorf("no instance available in all candidate zones"), "Error creating instance")
}

func (s *stepCreateAlicloudInstance) Cleanup(state multistep.StateBag) {
	if len(s.createdInstanceId) == 0 {
		return
	}
	cleanUpMessage(state, "instance")

	client := state.Get("client").(*ClientWrapper)
	ui := state.Get("ui").(packersdk.Ui)

	_, err := client.WaitForExpected(&WaitForExpectArgs{
		RequestFunc: func() (responses.AcsResponse, error) {
			request := ecs.CreateDeleteInstanceRequest()
			request.InstanceId = s.createdInstanceId
			request.Force = requests.NewBoolean(true)
			return client.DeleteInstance(request)
		},
		EvalFunc:   client.EvalCouldRetryResponse(deleteInstanceRetryErrors, EvalRetryErrorType),
		RetryTimes: shortRetryTimes,
	})

	if err != nil {
		ui.Say(fmt.Sprintf("Failed to clean up instance %s: %s", s.createdInstanceId, err))
	}
}

func (s *stepCreateAlicloudInstance) buildCreateInstanceRequest(state multistep.StateBag, vSwitch vpc.VSwitch) (*ecs.CreateInstanceRequest, error) {
	request := ecs.CreateCreateInstanceRequest()
	request.ClientToken = uuid.TimeOrderedUUID()
	request.RegionId = s.RegionId
	request.InstanceType = s.InstanceType
	request.InstanceName = s.InstanceName
	request.RamRoleName = s.RamRoleName
	request.Tag = buildCreateInstanceTags(s.Tags)
	request.ZoneId = vSwitch.ZoneId
	request.SecurityEnhancementStrategy = s.SecurityEnhancementStrategy
	if s.AlicloudImageFamily != "" {
		request.ImageFamily = s.AlicloudImageFamily
	} else {
		sourceImage := state.Get("source_image").(*ecs.Image)
		request.ImageId = sourceImage.ImageId
	}
	securityGroupId := state.Get("securitygroupid").(string)
	request.SecurityGroupId = securityGroupId

	networkType := state.Get("networktype").(InstanceNetWork)
	if networkType == InstanceNetworkVpc {
		request.VSwitchId = vSwitch.VSwitchId

		userData, err := s.getUserData()
		if err != nil {
			return nil, err
		}

		request.UserData = userData
	} else {
		if s.InternetChargeType == "" {
			s.InternetChargeType = "PayByTraffic"
		}

		if s.InternetMaxBandwidthOut == 0 {
			s.InternetMaxBandwidthOut = 5
		}
	}
	request.InternetChargeType = s.InternetChargeType
	request.InternetMaxBandwidthOut = requests.Integer(convertNumber(s.InternetMaxBandwidthOut))

	if s.IOOptimized.True() {
		request.IoOptimized = IOOptimizedOptimized
	} else if s.IOOptimized.False() {
		request.IoOptimized = IOOptimizedNone
	}

	config := state.Get("config").(*Config)
	password := config.Comm.SSHPassword
	if password == "" && config.Comm.WinRMPassword != "" {
		password = config.Comm.WinRMPassword
	}
	request.Password = password

	systemDisk := config.AlicloudImageConfig.ECSSystemDiskMapping
	request.SystemDiskDiskName = systemDisk.DiskName
	request.SystemDiskCategory = systemDisk.DiskCategory
	request.SystemDiskSize = requests.Integer(convertNumber(systemDisk.DiskSize))
	request.SystemDiskDescription = systemDisk.Description

	imageDisks := config.AlicloudImageConfig.ECSImagesDiskMappings
	var dataDisks []ecs.CreateInstanceDataDisk
	for _, imageDisk := range imageDisks {
		var dataDisk ecs.CreateInstanceDataDisk
		dataDisk.DiskName = imageDisk.DiskName
		dataDisk.Category = imageDisk.DiskCategory
		dataDisk.Size = convertNumber(imageDisk.DiskSize)
		dataDisk.SnapshotId = imageDisk.SnapshotId
		dataDisk.Description = imageDisk.Description
		dataDisk.DeleteWithInstance = strconv.FormatBool(imageDisk.DeleteWithInstance)
		dataDisk.Device = imageDisk.Device
		if imageDisk.Encrypted != confighelper.TriUnset {
			dataDisk.Encrypted = strconv.FormatBool(imageDisk.Encrypted.True())
		}

		dataDisks = append(dataDisks, dataDisk)
	}
	request.DataDisk = &dataDisks

	return request, nil
}

func (s *stepCreateAlicloudInstance) getUserData() (string, error) {
	userData := s.UserData

	if s.UserDataFile != "" {
		data, err := os.ReadFile(s.UserDataFile)
		if err != nil {
			return "", err
		}

		userData = string(data)
	}

	if userData != "" {
		userData = base64.StdEncoding.EncodeToString([]byte(userData))
	}

	return userData, nil

}

func buildCreateInstanceTags(tags map[string]string) *[]ecs.CreateInstanceTag {
	var ecsTags []ecs.CreateInstanceTag

	for k, v := range tags {
		ecsTags = append(ecsTags, ecs.CreateInstanceTag{Key: k, Value: v})
	}

	return &ecsTags
}
