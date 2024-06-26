// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package ecs

import (
	"testing"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
)

func testAlicloudImageConfig() *AlicloudImageConfig {
	return &AlicloudImageConfig{
		AlicloudImageName: "foo",
	}
}

func TestECSImageConfigPrepare_name(t *testing.T) {
	c := testAlicloudImageConfig()
	if err := c.Prepare(nil); err != nil {
		t.Fatalf("shouldn't have err: %s", err)
	}

	c.AlicloudImageName = ""
	if err := c.Prepare(nil); err == nil {
		t.Fatal("should have error")
	}
}

func TestAMIConfigPrepare_regions(t *testing.T) {
	c := testAlicloudImageConfig()
	c.AlicloudImageDestinationRegions = nil
	if err := c.Prepare(nil); err != nil {
		t.Fatalf("shouldn't have err: %s", err)
	}

	c.AlicloudImageDestinationRegions = []string{"cn-beijing", "cn-hangzhou", "eu-central-1"}
	if err := c.Prepare(nil); err != nil {
		t.Fatalf("bad: %s", err)
	}

	c.AlicloudImageDestinationRegions = nil
	if err := c.Prepare(nil); err != nil {
		t.Fatal("shouldn't have error")
	}
}

func TestECSImageConfigPrepare_imageTags(t *testing.T) {
	c := testAlicloudImageConfig()
	c.AlicloudImageTags = map[string]string{
		"TagKey1": "TagValue1",
		"TagKey2": "TagValue2",
	}
	if err := c.Prepare(nil); len(err) != 0 {
		t.Fatalf("err: %s", err)
	}
	if len(c.AlicloudImageTags) != 2 || c.AlicloudImageTags["TagKey1"] != "TagValue1" ||
		c.AlicloudImageTags["TagKey2"] != "TagValue2" {
		t.Fatalf("invalid value, expected: %s, actual: %s", map[string]string{
			"TagKey1": "TagValue1",
			"TagKey2": "TagValue2",
		}, c.AlicloudImageTags)
	}
}

func TestSkipIfExists(t *testing.T) {
	state := new(multistep.BasicStateBag)
	state.Put("error", ImageExistsError)
	if rawErr, ok := state.GetOk("error"); ok {
		if rawErr.(error) != ImageExistsError {
			t.Fatalf("shouldn't have err:%v", rawErr.(error))
		}
	}
}
