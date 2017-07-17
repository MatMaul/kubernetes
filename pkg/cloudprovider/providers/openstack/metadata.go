/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package openstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/util/exec"
	"k8s.io/kubernetes/pkg/util/mount"
)

// metadataUrl is URL to OpenStack metadata server. It's hardcoded IPv4
// link-local address as documented in "OpenStack Cloud Administrator Guide",
// chapter Compute - Networking with nova-network.
// http://docs.openstack.org/admin-guide-cloud/compute-networking-nova.html#metadata-service
const metadataUrl = "http://169.254.169.254/"
const metadataPath = "openstack/2012-08-10/meta_data.json"
const networkdataPath = "openstack/2015-10-15/network_data.json"

// Config drive is defined as an iso9660 or vfat (deprecated) drive
// with the "config-2" label.
// http://docs.openstack.org/user-guide/cli-config-drive.html
const configDriveLabel = "config-2"

var ErrBadMetadata = errors.New("Invalid OpenStack metadata, got empty uuid")

// Assumes the "2012-08-10" meta_data.json format.
// See http://docs.openstack.org/user-guide/cli_config_drive.html
type Metadata struct {
	Uuid             string `json:"uuid"`
	Name             string `json:"name"`
	AvailabilityZone string `json:"availability_zone"`
	// .. and other fields we don't care about.  Expand as necessary.
}

type Link struct {
	MAC  string `json:"ethernet_mac_address"`
	Type string `json:"type"`
	Id   string `json:"id"`
}

type Network struct {
	NetworkId string `json:"network_id"`
	IP        string `json:"ip_address"` // not available when type *_dhcp
	LinkId    string `json:"link"`
	Link      *Link  `json:"-"`
	Type      string `json:"type"`
}

type Service struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

type Networkdata struct {
	Links    []Link    `json:"links"`
	Networks []Network `json:"networks"`
	Services []Service `json:"services"`
	// .. and other fields we don't care about.  Expand as necessary.
}

// parseMetadataUUID reads JSON from OpenStack metadata server and parses
// instance ID out of it.
func parseMetadata(r io.Reader) (*Metadata, error) {
	var metadata Metadata
	json := json.NewDecoder(r)
	if err := json.Decode(&metadata); err != nil {
		return nil, err
	}

	if metadata.Uuid == "" {
		return nil, ErrBadMetadata
	}

	return &metadata, nil
}

func parseNetworkdata(r io.Reader) (*Networkdata, error) {
	var networkdata Networkdata
	json := json.NewDecoder(r)
	if err := json.Decode(&networkdata); err != nil {
		return nil, err
	}
	for _, network := range networkdata.Networks {
		for _, link := range networkdata.Links {
			if link.Id == network.LinkId {
				network.Link = &link
				break
			}
		}
	}

	return &networkdata, nil
}

func parseFullMetadata(mdReader, ndReader io.Reader) (*Metadata, *Networkdata, error) {
	md, err := parseMetadata(mdReader)
	if err != nil {
		return nil, nil, err
	}
	nd, err := parseNetworkdata(ndReader)
	if err != nil {
		glog.V(3).Infof("Can't parse network metadatas: %v", err)
	}
	return md, nd, nil
}

func getMetadataFromConfigDrive() (*Metadata, *Networkdata, error) {
	// Try to read instance UUID from config drive.
	dev := "/dev/disk/by-label/" + configDriveLabel
	if _, err := os.Stat(dev); os.IsNotExist(err) {
		out, err := exec.New().Command(
			"blkid", "-l",
			"-t", "LABEL="+configDriveLabel,
			"-o", "device",
		).CombinedOutput()
		if err != nil {
			glog.V(2).Infof("Unable to run blkid: %v", err)
			return nil, nil, err
		}
		dev = strings.TrimSpace(string(out))
	}

	mntdir, err := ioutil.TempDir("", "configdrive")
	if err != nil {
		return nil, nil, err
	}
	defer os.Remove(mntdir)

	glog.V(4).Infof("Attempting to mount configdrive %s on %s", dev, mntdir)

	mounter := mount.New("" /* default mount path */)
	err = mounter.Mount(dev, mntdir, "iso9660", []string{"ro"})
	if err != nil {
		err = mounter.Mount(dev, mntdir, "vfat", []string{"ro"})
	}
	if err != nil {
		glog.Errorf("Error mounting configdrive %s: %v", dev, err)
		return nil, nil, err
	}
	defer mounter.Unmount(mntdir)

	glog.V(4).Infof("Configdrive mounted on %s", mntdir)

	f, err := os.Open(
		filepath.Join(mntdir, metadataPath))
	if err != nil {
		glog.Errorf("Error reading %s on config drive: %v", metadataPath, err)
		return nil, nil, err
	}
	defer f.Close()
	f2, err2 := os.Open(
		filepath.Join(mntdir, networkdataPath))
	if err2 != nil {
		glog.Warningf("Error reading %s on config drive: %v", networkdataPath, err2)
		f2 = nil
	} else {
		defer f2.Close()
	}

	return parseFullMetadata(f, f2)
}

func getMetadataFromMetadataService() (*Metadata, *Networkdata, error) {
	// Try to get JSON from metdata server.
	url := metadataUrl + metadataPath
	glog.V(4).Infof("Attempting to fetch metadata from %s", url)
	mdBody, err := get(url)
	if err != nil {
		glog.V(3).Infof("Cannot read %s: %v", url, err)
		return nil, nil, err
	}
	url = metadataUrl + networkdataPath
	glog.V(4).Infof("Attempting to fetch network data from %s", url)
	ndBody, err := get(url)
	if err != nil {
		glog.V(3).Infof("Cannot read %s: %v", url, err)
	}

	return parseFullMetadata(mdBody, ndBody)
}

func get(url string) (io.Reader, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("Unexpected status code when reading metadata from %s: %s", url, resp.Status)
		glog.V(3).Infof("%v", err)
		return nil, err
	}
	return resp.Body, nil
}

// Metadata is fixed for the current host, so cache the value process-wide
var metadataCache *Metadata
var networkdataCache *Networkdata

func getMetadata() (*Metadata, *Networkdata, error) {
	if metadataCache == nil {
		md, nd, err := getMetadataFromConfigDrive()
		if err != nil {
			md, nd, err = getMetadataFromMetadataService()
		}
		if err != nil {
			return nil, nil, err
		}
		metadataCache = md
		networkdataCache = nd

	}
	return metadataCache, networkdataCache, nil
}
