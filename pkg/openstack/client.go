/*
Copyright (c) 2017 OpenStack Foundation.

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
	"errors"
	"fmt"
	"os"

	crv1 "git.openstack.org/openstack/stackube/pkg/apis/v1"
	crdClient "git.openstack.org/openstack/stackube/pkg/kubecrd"
	drivertypes "git.openstack.org/openstack/stackube/pkg/openstack/types"
	"git.openstack.org/openstack/stackube/pkg/util"

	"github.com/docker/distribution/uuid"
	"github.com/golang/glog"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/identity/v2/tenants"
	"github.com/gophercloud/gophercloud/openstack/identity/v2/users"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/portsbinding"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/rules"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"github.com/gophercloud/gophercloud/pagination"

	gcfg "gopkg.in/gcfg.v1"
)

const (
	StatusCodeAlreadyExists int = 409

	podNamePrefix     = "kube"
	securitygroupName = "kube-securitygroup-default"
	HostnameMaxLen    = 63

	// Service affinities
	ServiceAffinityNone     = "None"
	ServiceAffinityClientIP = "ClientIP"
)

var (
	adminStateUp = true

	ErrNotFound        = errors.New("NotFound")
	ErrMultipleResults = errors.New("MultipleResults")
)

// Interface should be implemented by a openstack client.
type Interface interface {
	// CreateTenant creates tenant by tenantname.
	CreateTenant(tenantName string) (string, error)
	// DeleteTenant deletes tenant by tenantName.
	DeleteTenant(tenantName string) error
	// GetTenantIDFromName gets tenantID by tenantName.
	GetTenantIDFromName(tenantName string) (string, error)
	// CheckTenantByID checks tenant exist or not by tenantID.
	CheckTenantByID(tenantID string) (bool, error)
	// CreateUser creates user with username, password in the tenant.
	CreateUser(username, password, tenantID string) error
	// DeleteAllUsersOnTenant deletes all users on the tenant.
	DeleteAllUsersOnTenant(tenantName string) error
	// CreateNetwork creates network.
	CreateNetwork(network *drivertypes.Network) error
	// GetNetworkByID gets network by networkID.
	GetNetworkByID(networkID string) (*drivertypes.Network, error)
	// GetNetworkByName gets network by networkName.
	GetNetworkByName(networkName string) (*drivertypes.Network, error)
	// DeleteNetwork deletes network by networkName.
	DeleteNetwork(networkName string) error
	// GetProviderSubnet gets provider subnet by id
	GetProviderSubnet(osSubnetID string) (*drivertypes.Subnet, error)
	// CreatePort creates port by neworkID, tenantID and portName.
	CreatePort(networkID, tenantID, portName string) (*portsbinding.Port, error)
	// GetPort gets port by portName.
	GetPort(name string) (*ports.Port, error)
	// ListPorts lists ports by networkID and deviceOwner.
	ListPorts(networkID, deviceOwner string) ([]ports.Port, error)
	// DeletePortByName deletes port by portName.
	DeletePortByName(portName string) error
	// DeletePortByID deletes port by portID.
	DeletePortByID(portID string) error
	// UpdatePortsBinding updates port binding.
	UpdatePortsBinding(portID, deviceOwner string) error
	// LoadBalancerExist returns whether a load balancer has already been exist.
	LoadBalancerExist(name string) (bool, error)
	// EnsureLoadBalancer ensures a load balancer is created.
	EnsureLoadBalancer(lb *LoadBalancer) (*LoadBalancerStatus, error)
	// EnsureLoadBalancerDeleted ensures a load balancer is deleted.
	EnsureLoadBalancerDeleted(name string) error
	// GetCRDClient returns the CRDClient.
	GetCRDClient() crdClient.Interface
	// GetPluginName returns the plugin name.
	GetPluginName() string
	// GetIntegrationBridge returns the integration bridge name.
	GetIntegrationBridge() string
}

// Client implements the openstack client Interface.
type Client struct {
	Identity          *gophercloud.ServiceClient
	Provider          *gophercloud.ProviderClient
	Network           *gophercloud.ServiceClient
	Region            string
	ExtNetID          string
	PluginName        string
	IntegrationBridge string
	CRDClient         crdClient.Interface
}

type PluginOpts struct {
	PluginName        string `gcfg:"plugin-name"`
	IntegrationBridge string `gcfg:"integration-bridge"`
}

// Config used to configure the openstack client.
type Config struct {
	Global struct {
		AuthUrl    string `gcfg:"auth-url"`
		Username   string `gcfg:"username"`
		Password   string `gcfg:"password"`
		TenantName string `gcfg:"tenant-name"`
		Region     string `gcfg:"region"`
		ExtNetID   string `gcfg:"ext-net-id"`
	}
	Plugin PluginOpts
}

func toAuthOptions(cfg Config) gophercloud.AuthOptions {
	return gophercloud.AuthOptions{
		IdentityEndpoint: cfg.Global.AuthUrl,
		Username:         cfg.Global.Username,
		Password:         cfg.Global.Password,
		TenantName:       cfg.Global.TenantName,
		AllowReauth:      true,
	}
}

// NewClient returns a new openstack client.
func NewClient(config string, kubeConfig string) (Interface, error) {
	var opts gophercloud.AuthOptions
	cfg, err := readConfig(config)
	if err != nil {
		return nil, fmt.Errorf("Failed read cloudconfig: %v", err)
	}
	glog.V(1).Infof("Initializing openstack client with config %v", cfg)

	if cfg.Global.ExtNetID == "" {
		return nil, fmt.Errorf("external network ID not set")
	}

	opts = toAuthOptions(cfg)
	provider, err := openstack.AuthenticatedClient(opts)
	if err != nil {
		return nil, err
	}

	identity, err := openstack.NewIdentityV2(provider, gophercloud.EndpointOpts{
		Availability: gophercloud.AvailabilityAdmin,
	})
	if err != nil {
		return nil, err
	}

	network, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{
		Region: cfg.Global.Region,
	})
	if err != nil {
		glog.Warning("Failed to find neutron endpoint: %v", err)
		return nil, err
	}

	// Create CRD client
	k8sConfig, err := util.NewClusterConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig: %v", err)
	}
	kubeCRDClient, err := crdClient.NewCRDClient(k8sConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create client for CRD: %v", err)
	}

	client := &Client{
		Identity:          identity,
		Provider:          provider,
		Network:           network,
		Region:            cfg.Global.Region,
		ExtNetID:          cfg.Global.ExtNetID,
		PluginName:        cfg.Plugin.PluginName,
		IntegrationBridge: cfg.Plugin.IntegrationBridge,
		CRDClient:         kubeCRDClient,
	}
	return client, nil
}

func readConfig(config string) (Config, error) {
	conf, err := os.Open(config)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	err = gcfg.ReadInto(&cfg, conf)
	if err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// GetCRDClient returns the CRDClient.
func (os *Client) GetCRDClient() crdClient.Interface {
	return os.CRDClient
}

// GetPluginName returns the plugin name.
func (os *Client) GetPluginName() string {
	return os.PluginName
}

// GetIntegrationBridge returns the integration bridge name.
func (os *Client) GetIntegrationBridge() string {
	return os.IntegrationBridge
}

// GetTenantIDFromName gets tenantID by tenantName.
func (os *Client) GetTenantIDFromName(tenantName string) (string, error) {
	if util.IsSystemNamespace(tenantName) {
		tenantName = util.SystemTenant
	}

	// If tenantID is specified, return it directly
	var (
		tenant *crv1.Tenant
		err    error
	)
	if tenant, err = os.CRDClient.GetTenant(tenantName); err != nil {
		return "", err
	}
	if tenant.Spec.TenantID != "" {
		return tenant.Spec.TenantID, nil
	}

	// Otherwise, fetch tenantID from OpenStack
	var tenantID string
	err = tenants.List(os.Identity, nil).EachPage(func(page pagination.Page) (bool, error) {
		tenantList, err1 := tenants.ExtractTenants(page)
		if err1 != nil {
			return false, err1
		}
		for _, t := range tenantList {
			if t.Name == tenantName {
				tenantID = t.ID
				break
			}
		}
		return true, nil
	})
	if err != nil {
		return "", err
	}

	glog.V(3).Infof("Got tenantID: %v for tenantName: %v", tenantID, tenantName)

	return tenantID, nil
}

// CreateTenant creates tenant by tenantname.
func (os *Client) CreateTenant(tenantName string) (string, error) {
	createOpts := tenants.CreateOpts{
		Name:        tenantName,
		Description: "stackube",
		Enabled:     gophercloud.Enabled,
	}

	_, err := tenants.Create(os.Identity, createOpts).Extract()
	if err != nil && !IsAlreadyExists(err) {
		glog.Errorf("Failed to create tenant %s: %v", tenantName, err)
		return "", err
	}
	glog.V(4).Infof("Tenant %s created", tenantName)
	tenantID, err := os.GetTenantIDFromName(tenantName)
	if err != nil {
		return "", err
	}
	return tenantID, nil
}

// DeleteTenant deletes tenant by tenantName.
func (os *Client) DeleteTenant(tenantName string) error {
	return tenants.List(os.Identity, nil).EachPage(func(page pagination.Page) (bool, error) {
		tenantList, err := tenants.ExtractTenants(page)
		if err != nil {
			return false, err
		}
		for _, t := range tenantList {
			if t.Name == tenantName {
				err := tenants.Delete(os.Identity, t.ID).ExtractErr()
				if err != nil {
					glog.Errorf("Delete openstack tenant %s error: %v", tenantName, err)
					return false, err
				}
				glog.V(4).Infof("Tenant %s deleted", tenantName)
				break
			}
		}
		return true, nil
	})
}

// CreateUser creates user with username, password in the tenant.
func (os *Client) CreateUser(username, password, tenantID string) error {
	opts := users.CreateOpts{
		Name:     username,
		TenantID: tenantID,
		Enabled:  gophercloud.Enabled,
		Password: password,
	}
	_, err := users.Create(os.Identity, opts).Extract()
	if err != nil && !IsAlreadyExists(err) {
		glog.Errorf("Failed to create user %s: %v", username, err)
		return err
	}
	glog.V(4).Infof("User %s created", username)
	return nil
}

// DeleteAllUsersOnTenant deletes all users on the tenant.
func (os *Client) DeleteAllUsersOnTenant(tenantName string) error {
	tenantID, err := os.GetTenantIDFromName(tenantName)
	if err != nil {
		return nil
	}
	return users.ListUsers(os.Identity, tenantID).EachPage(func(page pagination.Page) (bool, error) {
		usersList, err := users.ExtractUsers(page)
		if err != nil {
			return false, err
		}
		for _, u := range usersList {
			res := users.Delete(os.Identity, u.ID)
			if res.Err != nil {
				glog.Errorf("Delete openstack user %s error: %v", u.Name, err)
				return false, err
			}
			glog.V(4).Infof("User %s deleted", u.Name)
		}
		return true, nil
	})
}

// IsAlreadyExists determines if the err is an error which indicates that a specified resource already exists.
func IsAlreadyExists(err error) bool {
	return reasonForError(err) == StatusCodeAlreadyExists
}

func reasonForError(err error) int {
	switch t := err.(type) {
	case gophercloud.ErrUnexpectedResponseCode:
		return t.Actual
	}
	return 0
}

// GetOpenStackNetworkByTenantID gets tenant's network by tenantID(tenant and network are one to one mapping in stackube)
func (os *Client) GetOpenStackNetworkByTenantID(tenantID string) (*networks.Network, error) {
	opts := networks.ListOpts{TenantID: tenantID}
	return os.getOpenStackNetwork(&opts)
}

// Get openstack network by id
func (os *Client) getOpenStackNetworkByID(id string) (*networks.Network, error) {
	opts := networks.ListOpts{ID: id}
	return os.getOpenStackNetwork(&opts)
}

// Get openstack network by name
func (os *Client) getOpenStackNetworkByName(name string) (*networks.Network, error) {
	opts := networks.ListOpts{Name: name}
	return os.getOpenStackNetwork(&opts)
}

// Get openstack network
func (os *Client) getOpenStackNetwork(opts *networks.ListOpts) (*networks.Network, error) {
	var osNetwork *networks.Network
	pager := networks.List(os.Network, *opts)
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		networkList, e := networks.ExtractNetworks(page)
		if len(networkList) > 1 {
			return false, ErrMultipleResults
		}

		if len(networkList) == 1 {
			osNetwork = &networkList[0]
		}

		return true, e
	})

	if err == nil && osNetwork == nil {
		return nil, ErrNotFound
	}

	return osNetwork, err
}

// GetProviderSubnet gets provider subnet by subnetID
func (os *Client) GetProviderSubnet(osSubnetID string) (*drivertypes.Subnet, error) {
	s, err := subnets.Get(os.Network, osSubnetID).Extract()
	if err != nil {
		glog.Errorf("Get openstack subnet failed: %v", err)
		return nil, err
	}

	var routes []*drivertypes.Route
	for _, r := range s.HostRoutes {
		route := drivertypes.Route{
			Nexthop:         r.NextHop,
			DestinationCIDR: r.DestinationCIDR,
		}
		routes = append(routes, &route)
	}

	providerSubnet := drivertypes.Subnet{
		Uid:        s.ID,
		Cidr:       s.CIDR,
		Gateway:    s.GatewayIP,
		Name:       s.Name,
		Dnsservers: s.DNSNameservers,
		Routes:     routes,
	}

	return &providerSubnet, nil
}

// GetNetworkByID gets network by networkID
func (os *Client) GetNetworkByID(networkID string) (*drivertypes.Network, error) {
	osNetwork, err := os.getOpenStackNetworkByID(networkID)
	if err != nil {
		glog.Errorf("failed to fetch openstack network by iD: %v, failure: %v", networkID, err)
		return nil, err
	}

	return os.OSNetworktoProviderNetwork(osNetwork)
}

// GetNetworkByName gets network by networkName
func (os *Client) GetNetworkByName(networkName string) (*drivertypes.Network, error) {
	osNetwork, err := os.getOpenStackNetworkByName(networkName)
	if err != nil {
		glog.Warningf("try to fetch openstack network by name: %v but failed: %v", networkName, err)
		return nil, err
	}

	return os.OSNetworktoProviderNetwork(osNetwork)
}

// OSNetworktoProviderNetwork transfers networks.Network to drivertypes.Network.
func (os *Client) OSNetworktoProviderNetwork(osNetwork *networks.Network) (*drivertypes.Network, error) {
	var providerNetwork drivertypes.Network
	var providerSubnets []*drivertypes.Subnet
	providerNetwork.Name = osNetwork.Name
	providerNetwork.Uid = osNetwork.ID
	providerNetwork.Status = os.ToProviderStatus(osNetwork.Status)
	providerNetwork.TenantID = osNetwork.TenantID

	for _, subnetID := range osNetwork.Subnets {
		s, err := os.GetProviderSubnet(subnetID)
		if err != nil {
			return nil, err
		}
		providerSubnets = append(providerSubnets, s)
	}

	providerNetwork.Subnets = providerSubnets

	return &providerNetwork, nil
}

// ToProviderStatus transfers networks.Network's status to drivertypes.Network's status.
func (os *Client) ToProviderStatus(status string) string {
	switch status {
	case "ACTIVE":
		return "Active"
	case "BUILD":
		return "Pending"
	case "DOWN", "ERROR":
		return "Failed"
	default:
		return "Failed"
	}
}

// CreateNetwork creates network.
func (os *Client) CreateNetwork(network *drivertypes.Network) error {
	if len(network.Subnets) == 0 {
		return errors.New("Subnets is null")
	}

	// create network
	opts := networks.CreateOpts{
		Name:         network.Name,
		AdminStateUp: &adminStateUp,
		TenantID:     network.TenantID,
	}
	osNet, err := networks.Create(os.Network, opts).Extract()
	if err != nil {
		glog.Errorf("Create openstack network %s failed: %v", network.Name, err)
		return err
	}

	// create router
	routerOpts := routers.CreateOpts{
		// use network name as router name for convenience
		Name:        network.Name,
		TenantID:    network.TenantID,
		GatewayInfo: &routers.GatewayInfo{NetworkID: os.ExtNetID},
	}
	osRouter, err := routers.Create(os.Network, routerOpts).Extract()
	if err != nil {
		glog.Errorf("Create openstack router %s failed: %v", network.Name, err)
		delErr := os.DeleteNetwork(network.Name)
		if delErr != nil {
			glog.Errorf("Delete openstack network %s failed: %v", network.Name, delErr)
		}
		return err
	}

	// create subnets and connect them to router
	networkID := osNet.ID
	network.Status = os.ToProviderStatus(osNet.Status)
	network.Uid = osNet.ID
	for _, sub := range network.Subnets {
		// create subnet
		subnetOpts := subnets.CreateOpts{
			NetworkID:      networkID,
			CIDR:           sub.Cidr,
			Name:           sub.Name,
			IPVersion:      gophercloud.IPv4,
			TenantID:       network.TenantID,
			GatewayIP:      &sub.Gateway,
			DNSNameservers: sub.Dnsservers,
		}
		s, err := subnets.Create(os.Network, subnetOpts).Extract()
		if err != nil {
			glog.Errorf("Create openstack subnet %s failed: %v", sub.Name, err)
			delErr := os.DeleteNetwork(network.Name)
			if delErr != nil {
				glog.Errorf("Delete openstack network %s failed: %v", network.Name, delErr)
			}
			return err
		}

		// add subnet to router
		opts := routers.AddInterfaceOpts{
			SubnetID: s.ID,
		}
		_, err = routers.AddInterface(os.Network, osRouter.ID, opts).Extract()
		if err != nil {
			glog.Errorf("Create openstack subnet %s failed: %v", sub.Name, err)
			delErr := os.DeleteNetwork(network.Name)
			if delErr != nil {
				glog.Errorf("Delete openstack network %s failed: %v", network.Name, delErr)
			}
			return err
		}
	}

	return nil
}

// UpdateNetwork updates network.
func (os *Client) UpdateNetwork(network *drivertypes.Network) error {
	// TODO: update network subnets
	return nil
}

func (os *Client) getRouterByName(name string) (*routers.Router, error) {
	var result *routers.Router

	opts := routers.ListOpts{Name: name}
	pager := routers.List(os.Network, opts)
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		routerList, e := routers.ExtractRouters(page)
		if len(routerList) > 1 {
			return false, ErrMultipleResults
		} else if len(routerList) == 1 {
			result = &routerList[0]
		}

		return true, e
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// DeleteNetwork deletes network by networkName.
func (os *Client) DeleteNetwork(networkName string) error {
	osNetwork, err := os.getOpenStackNetworkByName(networkName)
	if err != nil {
		glog.Errorf("Get openstack network failed: %v", err)
		return err
	}

	if osNetwork != nil {
		// Delete ports
		opts := ports.ListOpts{NetworkID: osNetwork.ID}
		pager := ports.List(os.Network, opts)
		err := pager.EachPage(func(page pagination.Page) (bool, error) {
			portList, err := ports.ExtractPorts(page)
			if err != nil {
				glog.Errorf("Get openstack ports error: %v", err)
				return false, err
			}

			for _, port := range portList {
				if port.DeviceOwner == "network:router_interface" {
					continue
				}

				err = ports.Delete(os.Network, port.ID).ExtractErr()
				if err != nil {
					glog.Warningf("Delete port %v failed: %v", port.ID, err)
				}
			}

			return true, nil
		})
		if err != nil {
			glog.Errorf("Delete ports error: %v", err)
		}

		router, err := os.getRouterByName(networkName)
		if err != nil {
			glog.Errorf("Get openstack router %s error: %v", networkName, err)
			return err
		}

		// delete all subnets
		for _, subnet := range osNetwork.Subnets {
			if router != nil {
				opts := routers.RemoveInterfaceOpts{SubnetID: subnet}
				_, err := routers.RemoveInterface(os.Network, router.ID, opts).Extract()
				if err != nil {
					glog.Errorf("Get openstack router %s error: %v", networkName, err)
					return err
				}
			}

			err = subnets.Delete(os.Network, subnet).ExtractErr()
			if err != nil {
				glog.Errorf("Delete openstack subnet %s error: %v", subnet, err)
				return err
			}
		}

		// delete router
		if router != nil {
			err = routers.Delete(os.Network, router.ID).ExtractErr()
			if err != nil {
				glog.Errorf("Delete openstack router %s error: %v", router.ID, err)
				return err
			}
		}

		// delete network
		err = networks.Delete(os.Network, osNetwork.ID).ExtractErr()
		if err != nil {
			glog.Errorf("Delete openstack network %s error: %v", osNetwork.ID, err)
			return err
		}
	}

	return nil
}

// CheckTenantByID checks tenant exist or not by tenantID.
func (os *Client) CheckTenantByID(tenantID string) (bool, error) {
	opts := tenants.ListOpts{}
	pager := tenants.List(os.Identity, &opts)

	var found bool
	err := pager.EachPage(func(page pagination.Page) (bool, error) {

		tenantList, err := tenants.ExtractTenants(page)
		if err != nil {
			return false, err
		}

		if len(tenantList) == 0 {
			return false, ErrNotFound
		}

		for _, t := range tenantList {
			if t.ID == tenantID || t.Name == tenantID {
				found = true
			}
		}

		return true, nil
	})

	return found, err
}

// GetPort gets port by portName.
func (os *Client) GetPort(name string) (*ports.Port, error) {
	opts := ports.ListOpts{Name: name}
	pager := ports.List(os.Network, opts)

	var port *ports.Port
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		portList, err := ports.ExtractPorts(page)
		if err != nil {
			glog.Errorf("Get openstack ports error: %v", err)
			return false, err
		}

		if len(portList) > 1 {
			return false, ErrMultipleResults
		}

		if len(portList) == 0 {
			return false, ErrNotFound
		}

		port = &portList[0]

		return true, err
	})

	return port, err
}

func getHostName() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}

	return host
}

func (os *Client) ensureSecurityGroup(tenantID string) (string, error) {
	var securitygroup *groups.SecGroup

	opts := groups.ListOpts{
		TenantID: tenantID,
		Name:     securitygroupName,
	}
	pager := groups.List(os.Network, opts)
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		sg, err := groups.ExtractGroups(page)
		if err != nil {
			glog.Errorf("Get openstack securitygroups error: %v", err)
			return false, err
		}

		if len(sg) > 0 {
			securitygroup = &sg[0]
		}

		return true, err
	})
	if err != nil {
		return "", err
	}

	// If securitygroup doesn't exist, create a new one
	if securitygroup == nil {
		securitygroup, err = groups.Create(os.Network, groups.CreateOpts{
			Name:     securitygroupName,
			TenantID: tenantID,
		}).Extract()

		if err != nil {
			return "", err
		}
	}

	var secGroupsRules int
	listopts := rules.ListOpts{
		TenantID:   tenantID,
		Direction:  string(rules.DirIngress),
		SecGroupID: securitygroup.ID,
	}
	rulesPager := rules.List(os.Network, listopts)
	err = rulesPager.EachPage(func(page pagination.Page) (bool, error) {
		r, err := rules.ExtractRules(page)
		if err != nil {
			glog.Errorf("Get openstack securitygroup rules error: %v", err)
			return false, err
		}

		secGroupsRules = len(r)

		return true, err
	})
	if err != nil {
		return "", err
	}

	// create new rules
	if secGroupsRules == 0 {
		// create egress rule
		_, err = rules.Create(os.Network, rules.CreateOpts{
			TenantID:   tenantID,
			SecGroupID: securitygroup.ID,
			Direction:  rules.DirEgress,
			EtherType:  rules.EtherType4,
		}).Extract()

		// create ingress rule
		_, err := rules.Create(os.Network, rules.CreateOpts{
			TenantID:   tenantID,
			SecGroupID: securitygroup.ID,
			Direction:  rules.DirIngress,
			EtherType:  rules.EtherType4,
		}).Extract()
		if err != nil {
			return "", err
		}
	}

	return securitygroup.ID, nil
}

// CreatePort creates port by neworkID, tenantID and portName.
func (os *Client) CreatePort(networkID, tenantID, portName string) (*portsbinding.Port, error) {
	securitygroup, err := os.ensureSecurityGroup(tenantID)
	if err != nil {
		glog.Errorf("EnsureSecurityGroup failed: %v", err)
		return nil, err
	}

	opts := portsbinding.CreateOpts{
		HostID: getHostName(),
		CreateOptsBuilder: ports.CreateOpts{
			NetworkID:      networkID,
			Name:           portName,
			AdminStateUp:   &adminStateUp,
			TenantID:       tenantID,
			DeviceID:       uuid.Generate().String(),
			DeviceOwner:    fmt.Sprintf("compute:%s", getHostName()),
			SecurityGroups: []string{securitygroup},
		},
	}

	port, err := portsbinding.Create(os.Network, opts).Extract()
	if err != nil {
		glog.Errorf("Create port %s failed: %v", portName, err)
		return nil, err
	}
	return port, nil
}

// ListPorts lists ports by networkID and deviceOwner.
func (os *Client) ListPorts(networkID, deviceOwner string) ([]ports.Port, error) {
	var results []ports.Port
	opts := ports.ListOpts{
		NetworkID:   networkID,
		DeviceOwner: deviceOwner,
	}
	pager := ports.List(os.Network, opts)
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		portList, err := ports.ExtractPorts(page)
		if err != nil {
			glog.Errorf("Get openstack ports error: %v", err)
			return false, err
		}

		for _, port := range portList {
			results = append(results, port)
		}

		return true, err
	})

	if err != nil {
		return nil, err
	}

	return results, nil
}

// DeletePortByName deletes port by portName
func (os *Client) DeletePortByName(portName string) error {
	port, err := os.GetPort(portName)
	if err == util.ErrNotFound {
		glog.V(4).Infof("Port %s already deleted", portName)
		return nil
	} else if err != nil {
		glog.Errorf("Get openstack port %s failed: %v", portName, err)
		return err
	}

	if port != nil {
		err := ports.Delete(os.Network, port.ID).ExtractErr()
		if err != nil {
			glog.Errorf("Delete openstack port %s failed: %v", portName, err)
			return err
		}
	}

	return nil
}

// DeletePortByID deletes port by portID.
func (os *Client) DeletePortByID(portID string) error {
	err := ports.Delete(os.Network, portID).ExtractErr()
	if err != nil {
		glog.Errorf("Delete openstack port portID %s failed: %v", portID, err)
		return err
	}

	return nil
}

// UpdatePortsBinding updates port binding.
func (os *Client) UpdatePortsBinding(portID, deviceOwner string) error {
	// Update hostname in order to make sure it is correct
	updateOpts := portsbinding.UpdateOpts{
		HostID: getHostName(),
		UpdateOptsBuilder: ports.UpdateOpts{
			DeviceOwner: deviceOwner,
		},
	}
	_, err := portsbinding.Update(os.Network, portID, updateOpts).Extract()
	return err
}
