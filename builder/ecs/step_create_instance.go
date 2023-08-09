// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package ecs

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/errors"
	"github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	"io/ioutil"
	"strconv"

	"github.com/hashicorp/packer-plugin-sdk/uuid"

	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/requests"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/responses"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/ecs"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	confighelper "github.com/hashicorp/packer-plugin-sdk/template/config"
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
	ZoneId                      string
	SecurityEnhancementStrategy string
	AlicloudImageFamily         string
	instance                    *ecs.Instance
	NoCleanUp                   bool
}

var createInstanceRetryErrors = []string{
	"IdempotentProcessing",
}

var deleteInstanceRetryErrors = []string{
	"IncorrectInstanceStatus.Initializing",
}

func (s *stepCreateAlicloudInstance) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	client := state.Get("client").(*ClientWrapper)
	ui := state.Get("ui").(packersdk.Ui)

	ui.Say("Creating instance...")
	createInstanceRequest, err := s.buildCreateInstanceRequest(state)
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
		// 根据错误码判断是否是库存不足的错误
		// 如果是库存不足的错误，查询可用区后换区重试
		e, ok := err.(errors.Error)
		if !ok || e.ErrorCode() != "OperationDenied.NoStock" {
			config := state.Get("config").(*Config)
			newReq := ecs.CreateDescribeRecommendInstanceTypeRequest()
			newReq.RegionId = s.RegionId
			newReq.NetworkType = "vpc"
			newReq.InstanceChargeType = "PostPaid"
			newReq.SystemDiskCategory = "cloud_essd"
			newReq.InstanceType = s.InstanceType
			newReq.IoOptimized = "optimized"
			newReq.PriorityStrategy = "PriceFirst"
			newReq.InstanceChargeType = "PostPaid"
			newReq.Scene = "CREATE"
			newReq.SpotStrategy = "NoSpot"

			createInstanceResponse, err := client.WaitForExpected(&WaitForExpectArgs{
				RequestFunc: func() (responses.AcsResponse, error) {
					return client.DescribeRecommendInstanceType(newReq)
				},
				EvalFunc: client.EvalCouldRetryResponse(createInstanceRetryErrors, EvalRetryErrorType),
			})
			if err != nil {
				return halt(state, err, "Error auto re-arrange instance zone")
			}
			for _, instanceType := range createInstanceResponse.(*ecs.DescribeRecommendInstanceTypeResponse).Data.RecommendInstanceType {
				s.ZoneId = instanceType.ZoneId
				ui.Say(fmt.Sprintf("Instance type %s is not available in zone %s, try to use %s", s.InstanceType, config.ZoneId, s.ZoneId))
				steps := []multistep.Step{
					&stepConfigAlicloudVSwitch{
						VSwitchId:   config.VSwitchId,
						ZoneId:      s.ZoneId,
						CidrBlock:   config.CidrBlock,
						VSwitchName: config.VSwitchName,
					},
					&stepCreateAlicloudInstance{
						IOOptimized:                 config.IOOptimized,
						InstanceType:                config.InstanceType,
						UserData:                    config.UserData,
						UserDataFile:                config.UserDataFile,
						RamRoleName:                 config.RamRoleName,
						Tags:                        config.RunTags,
						RegionId:                    config.AlicloudRegion,
						InternetChargeType:          config.InternetChargeType,
						InternetMaxBandwidthOut:     config.InternetMaxBandwidthOut,
						InstanceName:                config.InstanceName,
						ZoneId:                      s.ZoneId,
						SecurityEnhancementStrategy: config.SecurityEnhancementStrategy,
						AlicloudImageFamily:         config.AlicloudImageFamily,
						NoCleanUp:                   true,
					},
				}
				runner := commonsteps.NewRunner(steps, config.PackerConfig, ui)
				runner.Run(ctx, state)
				config.ZoneId = s.ZoneId
				s.instance = state.Get("instance").(*ecs.Instance)
				return multistep.ActionContinue
			}
		}
		return halt(state, err, "Error creating instance")
	}

	instanceId := createInstanceResponse.(*ecs.CreateInstanceResponse).InstanceId

	_, err = client.WaitForInstanceStatus(s.RegionId, instanceId, InstanceStatusStopped)
	if err != nil {
		return halt(state, err, "Error waiting create instance")
	}

	describeInstancesRequest := ecs.CreateDescribeInstancesRequest()
	describeInstancesRequest.InstanceIds = fmt.Sprintf("[\"%s\"]", instanceId)
	instances, err := client.DescribeInstances(describeInstancesRequest)
	if err != nil {
		return halt(state, err, "")
	}

	ui.Message(fmt.Sprintf("Created instance: %s", instanceId))
	s.instance = &instances.Instances.Instance[0]
	state.Put("instance", s.instance)
	// instance_id is the generic term used so that users can have access to the
	// instance id inside of the provisioners, used in step_provision.
	state.Put("instance_id", instanceId)

	return multistep.ActionContinue
}

func (s *stepCreateAlicloudInstance) Cleanup(state multistep.StateBag) {
	if s.instance == nil {
		return
	}
	cleanUpMessage(state, "instance")

	client := state.Get("client").(*ClientWrapper)
	ui := state.Get("ui").(packersdk.Ui)

	_, err := client.WaitForExpected(&WaitForExpectArgs{
		RequestFunc: func() (responses.AcsResponse, error) {
			request := ecs.CreateDeleteInstanceRequest()
			request.InstanceId = s.instance.InstanceId
			request.Force = requests.NewBoolean(true)
			return client.DeleteInstance(request)
		},
		EvalFunc:   client.EvalCouldRetryResponse(deleteInstanceRetryErrors, EvalRetryErrorType),
		RetryTimes: shortRetryTimes,
	})

	if err != nil {
		ui.Say(fmt.Sprintf("Failed to clean up instance %s: %s", s.instance.InstanceId, err))
	}
}

func (s *stepCreateAlicloudInstance) buildCreateInstanceRequest(state multistep.StateBag) (*ecs.CreateInstanceRequest, error) {
	request := ecs.CreateCreateInstanceRequest()
	request.ClientToken = uuid.TimeOrderedUUID()
	request.RegionId = s.RegionId
	request.InstanceType = s.InstanceType
	request.InstanceName = s.InstanceName
	request.RamRoleName = s.RamRoleName
	request.Tag = buildCreateInstanceTags(s.Tags)
	request.ZoneId = s.ZoneId
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
		vswitchId := state.Get("vswitchid").(string)
		request.VSwitchId = vswitchId

		userData, err := s.getUserData(state)
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

func (s *stepCreateAlicloudInstance) getUserData(state multistep.StateBag) (string, error) {
	userData := s.UserData

	if s.UserDataFile != "" {
		data, err := ioutil.ReadFile(s.UserDataFile)
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
