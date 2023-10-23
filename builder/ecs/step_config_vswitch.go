// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package ecs

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/responses"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/ecs"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/vpc"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/uuid"
)

type stepConfigAlicloudVSwitch struct {
	VSwitchId        string
	ZoneId           string
	CidrBlock        string
	VSwitchName      string
	createdVSwitchId string
}

var createVSwitchRetryErrors = []string{
	"TOKEN_PROCESSING",
}

var deleteVSwitchRetryErrors = []string{
	"IncorrectVSwitchStatus",
	"DependencyViolation",
	"DependencyViolation.HaVip",
	"IncorrectRouteEntryStatus",
	"TaskConflict",
}

func (s *stepConfigAlicloudVSwitch) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	client := state.Get("client").(*ClientWrapper)
	vpcClient := state.Get("vpcClient").(*VPCClientWrapper)
	ui := state.Get("ui").(packersdk.Ui)
	vpcId := state.Get("vpcid").(string)
	config := state.Get("config").(*Config)

	// 指定交换机
	if len(s.VSwitchId) > 0 {
		// 查询交换机是否存在并获取详细信息
		describeVSwitchesRequest := vpc.CreateDescribeVSwitchesRequest()
		describeVSwitchesRequest.VpcId = vpcId
		describeVSwitchesRequest.VSwitchId = s.VSwitchId
		describeVSwitchesRequest.VSwitchName = s.VSwitchName
		describeVSwitchesRequest.ZoneId = s.ZoneId

		vswitchesResponse, err := vpcClient.DescribeVSwitches(describeVSwitchesRequest)
		if err != nil {
			return halt(state, err, "Failed querying vswitch")
		}

		vswitch := vswitchesResponse.VSwitches.VSwitch
		if len(vswitch) == 1 {
			state.Put("vswitches", vswitch)
			return multistep.ActionContinue
		}
		return halt(state, fmt.Errorf("the specified vswitch {%s} doesn't exist", s.VSwitchId), "")
	}

	// 根据机型自动选择可用区
	zones := []string{s.ZoneId}
	if len(s.ZoneId) == 0 {
		ui.Say("Searching zones...")
		availableResourceRequest := ecs.CreateDescribeAvailableResourceRequest()
		availableResourceRequest.RegionId = config.AlicloudRegion
		availableResourceRequest.DestinationResource = "InstanceType"
		availableResourceRequest.IoOptimized = "optimized"
		availableResourceRequest.ResourceType = "instance"
		availableResourceRequest.InstanceType = config.InstanceType

		resourceResponse, err := client.DescribeAvailableResource(availableResourceRequest)
		if err != nil {
			return halt(state, err, "Query for available instance zones failed")
		}

		zones = make([]string, 0)
		for _, zone := range resourceResponse.AvailableZones.AvailableZone {
			if zone.Status == "Available" &&
				zone.AvailableResources.AvailableResource[0].SupportedResources.SupportedResource[0].Status == "Available" {
				zones = append(zones, zone.ZoneId)
			}
		}
		if len(zones) == 0 {
			ui.Say(fmt.Sprintf("实例类型 %s 没有在售可用区", config.InstanceType))
			state.Put("error", fmt.Errorf("实例类型 %s 没有在售可用区", config.InstanceType))
			return multistep.ActionHalt
		}
		ui.Say("Candidate zones are: " + strings.Join(zones, ", "))
	}

	// 根据机型自动选择可用区和交换机
	if len(s.VSwitchName) != 0 {
		ui.Say(fmt.Sprintf("Searching vswitches using name: %s ...", s.VSwitchName))
		// 搜索机型在售所有可用区内符合subnet名称的subnet
		// 由于不可以指定可用区列表，因此需要遍历返回值然后进行过滤
		describeVSwitchesRequest := vpc.CreateDescribeVSwitchesRequest()
		describeVSwitchesRequest.VpcId = vpcId
		describeVSwitchesRequest.VSwitchName = s.VSwitchName

		vSwitchesResponse, err := vpcClient.DescribeVSwitches(describeVSwitchesRequest)
		if err != nil {
			return halt(state, err, "Failed querying vswitch")
		}

		vSwitchCandidates := vSwitchesResponse.VSwitches.VSwitch
		if len(vSwitchCandidates) > 0 {
			vSwitches := make([]vpc.VSwitch, 0)
			for _, v := range vSwitchCandidates {
				if slices.Contains(zones, v.ZoneId) {
					vSwitches = append(vSwitches, v)
				}
			}
			state.Put("vswitches", vSwitches)
			return multistep.ActionContinue
		}

		return halt(state, fmt.Errorf("the specified vswitch {%s} doesn't exist", s.VSwitchName), "")
	}

	if config.CidrBlock == "" {
		s.CidrBlock = DefaultCidrBlock //use the default CirdBlock
	}

	ui.Say("Creating vswitch...")
	// 由于cidr段无法有效指定多个，因此建立vsw只建立第一个成功的可用区
	for _, zoneId := range zones {
		ui.Say("Try to create vsw in zone: " + zoneId)
		createVSwitchRequest := vpc.CreateCreateVSwitchRequest()
		createVSwitchRequest.ClientToken = uuid.TimeOrderedUUID()
		createVSwitchRequest.CidrBlock = s.CidrBlock
		createVSwitchRequest.ZoneId = zoneId
		createVSwitchRequest.VpcId = vpcId
		createVSwitchRequest.VSwitchName = s.VSwitchName
		createVSwitchResponse, err := client.WaitForExpected(&WaitForExpectArgs{
			RequestFunc: func() (responses.AcsResponse, error) {
				return vpcClient.CreateVSwitch(createVSwitchRequest)
			},
			EvalFunc: client.EvalCouldRetryResponse(createVSwitchRetryErrors, EvalRetryErrorType),
		})
		if err != nil {
			ui.Say("Error creating vswitch: " + err.Error())
			continue
		}

		s.createdVSwitchId = createVSwitchResponse.(*vpc.CreateVSwitchResponse).VSwitchId

		describeVSwitchesRequest := vpc.CreateDescribeVSwitchesRequest()
		describeVSwitchesRequest.VpcId = vpcId
		describeVSwitchesRequest.VSwitchId = s.createdVSwitchId

		var vswitch vpc.VSwitch
		_, err = client.WaitForExpected(&WaitForExpectArgs{
			RequestFunc: func() (responses.AcsResponse, error) {
				return vpcClient.DescribeVSwitches(describeVSwitchesRequest)
			},
			EvalFunc: func(response responses.AcsResponse, err error) WaitForExpectEvalResult {
				if err != nil {
					return WaitForExpectToRetry
				}

				vSwitchesResponse := response.(*vpc.DescribeVSwitchesResponse)
				vSwitches := vSwitchesResponse.VSwitches.VSwitch
				if len(vSwitches) > 0 {
					for _, vSwitch := range vSwitches {
						if vSwitch.Status == VSwitchStatusAvailable {
							vswitch = vSwitch
							return WaitForExpectSuccess
						}
					}
				}
				return WaitForExpectToRetry
			},
			RetryTimes: shortRetryTimes,
		})

		if err != nil {
			return halt(state, err, "Timeout waiting for vswitch to become available")
		}
		ui.Message(fmt.Sprintf("Created vswitch: %s", s.createdVSwitchId))

		state.Put("vswitches", []vpc.VSwitch{vswitch})
		return multistep.ActionContinue
	}
	return halt(state, fmt.Errorf("no vswitch created successfully in all candidate zones"), "Error creating vswitch")
}

func (s *stepConfigAlicloudVSwitch) Cleanup(state multistep.StateBag) {
	if len(s.createdVSwitchId) == 0 {
		return
	}

	cleanUpMessage(state, "vSwitch")

	client := state.Get("client").(*ClientWrapper)
	ui := state.Get("ui").(packersdk.Ui)

	_, err := client.WaitForExpected(&WaitForExpectArgs{
		RequestFunc: func() (responses.AcsResponse, error) {
			request := ecs.CreateDeleteVSwitchRequest()
			request.VSwitchId = s.createdVSwitchId
			return client.DeleteVSwitch(request)
		},
		EvalFunc:   client.EvalCouldRetryResponse(deleteVSwitchRetryErrors, EvalRetryErrorType),
		RetryTimes: shortRetryTimes,
	})

	if err != nil {
		ui.Error(fmt.Sprintf("Error deleting vswitch, it may still be around: %s", err))
	}

}
