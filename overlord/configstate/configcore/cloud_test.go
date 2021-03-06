// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2017 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package configcore_test

import (
	"io/ioutil"
	"os"
	"path/filepath"

	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/overlord/configstate/configcore"
)

type cloudSuite struct {
	configcoreSuite
}

var _ = Suite(&cloudSuite{})

func (s *cloudSuite) SetUpTest(c *C) {
	s.configcoreSuite.SetUpTest(c)
	dirs.SetRootDir(c.MkDir())
}

func (s *cloudSuite) TearDownTest(c *C) {
	dirs.SetRootDir("/")
}

func (s *cloudSuite) TestHandleCloud(c *C) {
	tests := []struct {
		instData         string
		name             string
		region           string
		availabilityZone string
	}{
		{"", "", "", ""},
		{`{
}`, "", "", ""},
		{"{", "", "", ""},
		{`{
 "v1": {
  "availability-zone": "us-east-2b",
  "cloud-name": "aws",
  "instance-id": "i-03bdbe0d89f4c8ec9",
  "local-hostname": "ip-10-41-41-143",
  "region": "us-east-2"
 }
}`, "aws", "us-east-2", "us-east-2b"},
		{`{
"v1": {
  "availability-zone": null,
  "cloud-name": "azure",
  "instance-id": "1C63DFBB-46A0-7243-A11F-5A1F6EAEBCCB",
  "public-hostname": "my-b1",
  "public-ipv4-address": null,
  "public-ipv6-address": null,
  "region": null
 }
}`, "azure", "", ""},
		{`{
 "v1": {
  "availability-zone": "nova",
  "cloud-name": "openstack",
  "instance-id": "3e39d278-0644-4728-9479-678f9212d8f0",
  "local-hostname": "xenial-test",
  "region": null
 }
}`, "openstack", "", "nova"},
		{`{
 "v1": {
  "availability-zone": null,
  "cloud-name": "nocloud",
  "instance-id": "b5",
  "local-hostname": "b5",
  "region": null
 }
}`, "", "", ""},
		{},
		{`{
  "v1": null,
}`, "", "", ""},
		{`{
 "v1": {
   "cloud-name": "none"
 }
}`, "", "", ""},
	}

	err := os.MkdirAll(filepath.Dir(dirs.CloudInstanceDataFile), 0755)
	c.Assert(err, IsNil)

	for _, t := range tests {
		os.Remove(dirs.CloudInstanceDataFile)
		if t.instData != "" {
			err = ioutil.WriteFile(dirs.CloudInstanceDataFile, []byte(t.instData), 0600)
			c.Assert(err, IsNil)
		}

		tr := &mockConf{
			state: s.state,
		}
		err := configcore.Run(tr)
		c.Assert(err, IsNil)

		var cloudInfo configcore.CloudInfo
		tr.Get("core", "cloud", &cloudInfo)

		c.Check(cloudInfo.Name, Equals, t.name)
		c.Check(cloudInfo.Region, Equals, t.region)
		c.Check(cloudInfo.AvailabilityZone, Equals, t.availabilityZone)
	}
}

func (s *cloudSuite) TestHandleCloudAlreadySeeded(c *C) {
	instData := `{
 "v1": {
  "availability-zone": "us-east-2b",
  "cloud-name": "aws",
  "instance-id": "i-03bdbe0d89f4c8ec9",
  "local-hostname": "ip-10-41-41-143",
  "region": "us-east-2"
 }
}`

	err := os.MkdirAll(filepath.Dir(dirs.CloudInstanceDataFile), 0755)
	c.Assert(err, IsNil)
	err = ioutil.WriteFile(dirs.CloudInstanceDataFile, []byte(instData), 0600)
	c.Assert(err, IsNil)

	s.state.Lock()
	s.state.Set("seeded", true)
	s.state.Unlock()
	tr := &mockConf{
		state: s.state,
	}
	err = configcore.Run(tr)
	c.Assert(err, IsNil)

	var cloudInfo configcore.CloudInfo
	tr.Get("core", "cloud", &cloudInfo)

	c.Check(cloudInfo.Name, Equals, "")
}
