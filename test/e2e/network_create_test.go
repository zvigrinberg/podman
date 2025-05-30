package integration

import (
	"encoding/json"
	"net"

	"github.com/containers/common/libnetwork/types"
	. "github.com/containers/podman/v4/test/utils"
	"github.com/containers/storage/pkg/stringid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gexec"
)

func removeNetworkDevice(name string) {
	session := SystemExec("ip", []string{"link", "delete", name})
	session.WaitWithDefaultTimeout()
}

var _ = Describe("Podman network create", func() {

	It("podman network create with name and subnet", func() {
		netName := "subnet-" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{"network", "create", "--subnet", "10.11.12.0/24", "--ip-range", "10.11.12.0/26", netName})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName)
		Expect(nc).Should(Exit(0))

		// Inspect the network configuration
		inspect := podmanTest.Podman([]string{"network", "inspect", netName})
		inspect.WaitWithDefaultTimeout()
		Expect(inspect).Should(Exit(0))

		// JSON the network configuration into something usable
		var results []types.Network
		err := json.Unmarshal([]byte(inspect.OutputToString()), &results)
		Expect(err).ToNot(HaveOccurred())
		Expect(results).To(HaveLen(1))
		result := results[0]
		Expect(result).To(HaveField("Name", netName))
		Expect(result.Subnets).To(HaveLen(1))
		Expect(result.Subnets[0].Subnet.String()).To(Equal("10.11.12.0/24"))
		Expect(result.Subnets[0].Gateway.String()).To(Equal("10.11.12.1"))
		Expect(result.Subnets[0].LeaseRange).ToNot(BeNil())
		Expect(result.Subnets[0].LeaseRange.StartIP.String()).To(Equal("10.11.12.1"))
		Expect(result.Subnets[0].LeaseRange.EndIP.String()).To(Equal("10.11.12.63"))

		// Once a container executes a new network, the nic will be created. We should clean those up
		// best we can
		defer removeNetworkDevice(result.NetworkInterface)

		try := podmanTest.Podman([]string{"run", "--rm", "--network", netName, ALPINE, "sh", "-c", "ip addr show eth0 |  awk ' /inet / {print $2}'"})
		try.WaitWithDefaultTimeout()
		Expect(try).To(Exit(0))

		_, subnet, err := net.ParseCIDR("10.11.12.0/24")
		Expect(err).ToNot(HaveOccurred())
		// Note this is an IPv4 test only!
		containerIP, _, err := net.ParseCIDR(try.OutputToString())
		Expect(err).ToNot(HaveOccurred())
		// Ensure that the IP the container got is within the subnet the user asked for
		Expect(subnet.Contains(containerIP)).To(BeTrue(), "subnet contains containerIP")
	})

	It("podman network create with name and subnet and static route", func() {
		SkipIfCNI(podmanTest)
		netName := "subnet-" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{
			"network",
			"create",
			"--subnet",
			"10.19.12.0/24",
			"--route",
			"10.21.0.0/24,10.19.12.250",
			netName,
		})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName)
		Expect(nc).Should(Exit(0))

		// Inspect the network configuration
		inspect := podmanTest.Podman([]string{"network", "inspect", netName})
		inspect.WaitWithDefaultTimeout()
		Expect(inspect).Should(Exit(0))

		// JSON the network configuration into something usable
		var results []types.Network
		err := json.Unmarshal([]byte(inspect.OutputToString()), &results)
		Expect(err).ToNot(HaveOccurred())
		Expect(results).To(HaveLen(1))
		result := results[0]
		Expect(result).To(HaveField("Name", netName))
		Expect(result.Subnets).To(HaveLen(1))
		Expect(result.Subnets[0].Subnet.String()).To(Equal("10.19.12.0/24"))
		Expect(result.Subnets[0].Gateway.String()).To(Equal("10.19.12.1"))
		Expect(result.Routes[0].Destination.String()).To(Equal("10.21.0.0/24"))
		Expect(result.Routes[0].Gateway.String()).To(Equal("10.19.12.250"))
		Expect(result.Routes[0].Metric).To(BeNil())

		// Once a container executes a new network, the nic will be created. We should clean those up
		// best we can
		defer removeNetworkDevice(result.NetworkInterface)

	})

	It("podman network create with name and subnet and static route and metric", func() {
		SkipIfCNI(podmanTest)
		netName := "subnet-" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{
			"network",
			"create",
			"--subnet",
			"10.19.13.0/24",
			"--route",
			"10.21.1.0/24,10.19.13.250,120",
			netName,
		})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName)
		Expect(nc).Should(Exit(0))

		// Inspect the network configuration
		inspect := podmanTest.Podman([]string{"network", "inspect", netName})
		inspect.WaitWithDefaultTimeout()
		Expect(inspect).Should(Exit(0))

		// JSON the network configuration into something usable
		var results []types.Network
		err := json.Unmarshal([]byte(inspect.OutputToString()), &results)
		Expect(err).ToNot(HaveOccurred())
		Expect(results).To(HaveLen(1))
		result := results[0]
		Expect(result).To(HaveField("Name", netName))
		Expect(result.Subnets).To(HaveLen(1))
		Expect(result.Subnets[0].Subnet.String()).To(Equal("10.19.13.0/24"))
		Expect(result.Subnets[0].Gateway.String()).To(Equal("10.19.13.1"))
		Expect(result.Routes[0].Destination.String()).To(Equal("10.21.1.0/24"))
		Expect(result.Routes[0].Gateway.String()).To(Equal("10.19.13.250"))
		Expect(*result.Routes[0].Metric).To(Equal(uint32(120)))

		// Once a container executes a new network, the nic will be created. We should clean those up
		// best we can
		defer removeNetworkDevice(result.NetworkInterface)

	})

	It("podman network create with name and subnet and two static routes", func() {
		SkipIfCNI(podmanTest)
		netName := "subnet-" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{
			"network",
			"create",
			"--subnet",
			"10.19.14.0/24",
			"--route",
			"10.21.2.0/24,10.19.14.250",
			"--route",
			"10.21.3.0/24,10.19.14.251,120",
			netName,
		})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName)
		Expect(nc).Should(Exit(0))

		// Inspect the network configuration
		inspect := podmanTest.Podman([]string{"network", "inspect", netName})
		inspect.WaitWithDefaultTimeout()
		Expect(inspect).Should(Exit(0))

		// JSON the network configuration into something usable
		var results []types.Network
		err := json.Unmarshal([]byte(inspect.OutputToString()), &results)
		Expect(err).ToNot(HaveOccurred())
		Expect(results).To(HaveLen(1))
		result := results[0]
		Expect(result).To(HaveField("Name", netName))
		Expect(result.Subnets).To(HaveLen(1))
		Expect(result.Subnets[0].Subnet.String()).To(Equal("10.19.14.0/24"))
		Expect(result.Subnets[0].Gateway.String()).To(Equal("10.19.14.1"))
		Expect(result.Routes).To(HaveLen(2))
		Expect(result.Routes[0].Destination.String()).To(Equal("10.21.2.0/24"))
		Expect(result.Routes[0].Gateway.String()).To(Equal("10.19.14.250"))
		Expect(result.Routes[0].Metric).To(BeNil())
		Expect(result.Routes[1].Destination.String()).To(Equal("10.21.3.0/24"))
		Expect(result.Routes[1].Gateway.String()).To(Equal("10.19.14.251"))
		Expect(*result.Routes[1].Metric).To(Equal(uint32(120)))

		// Once a container executes a new network, the nic will be created. We should clean those up
		// best we can
		defer removeNetworkDevice(result.NetworkInterface)

	})

	It("podman network create with name and subnet and static route (ipv6)", func() {
		SkipIfCNI(podmanTest)
		netName := "subnet-" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{
			"network",
			"create",
			"--subnet",
			"fd:ab04::/64",
			"--route",
			"fd:1::/64,fd::1,120",
			netName,
		})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName)
		Expect(nc).Should(Exit(0))

		// Inspect the network configuration
		inspect := podmanTest.Podman([]string{"network", "inspect", netName})
		inspect.WaitWithDefaultTimeout()
		Expect(inspect).Should(Exit(0))

		// JSON the network configuration into something usable
		var results []types.Network
		err := json.Unmarshal([]byte(inspect.OutputToString()), &results)
		Expect(err).ToNot(HaveOccurred())
		Expect(results).To(HaveLen(1))
		result := results[0]
		Expect(result).To(HaveField("Name", netName))
		Expect(result.Subnets).To(HaveLen(1))
		Expect(result.Subnets[0].Subnet.String()).To(Equal("fd:ab04::/64"))
		Expect(result.Subnets[0].Gateway.String()).To(Equal("fd:ab04::1"))
		Expect(result.Routes[0].Destination.String()).To(Equal("fd:1::/64"))
		Expect(result.Routes[0].Gateway.String()).To(Equal("fd::1"))
		Expect(*result.Routes[0].Metric).To(Equal(uint32(120)))

		// Once a container executes a new network, the nic will be created. We should clean those up
		// best we can
		defer removeNetworkDevice(result.NetworkInterface)

	})

	It("podman network create with name and subnet with --opt no_default_route=1", func() {
		SkipIfCNI(podmanTest)
		netName := "subnet-" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{
			"network",
			"create",
			"--subnet",
			"10.19.15.0/24",
			"--opt",
			"no_default_route=1",
			netName,
		})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName)
		Expect(nc).Should(Exit(0))

		// Inspect the network configuration
		inspect := podmanTest.Podman([]string{"network", "inspect", netName})
		inspect.WaitWithDefaultTimeout()
		Expect(inspect).Should(Exit(0))

		// JSON the network configuration into something usable
		var results []types.Network
		err := json.Unmarshal([]byte(inspect.OutputToString()), &results)
		Expect(err).ToNot(HaveOccurred())
		Expect(results).To(HaveLen(1))
		result := results[0]
		Expect(result).To(HaveField("Name", netName))
		Expect(result.Subnets).To(HaveLen(1))
		Expect(result.Subnets[0].Subnet.String()).To(Equal("10.19.15.0/24"))
		Expect(result.Subnets[0].Gateway.String()).To(Equal("10.19.15.1"))
		Expect(result.Options[types.NoDefaultRoute]).To(Equal("true"))

		// Once a container executes a new network, the nic will be created. We should clean those up
		// best we can
		defer removeNetworkDevice(result.NetworkInterface)

	})

	It("podman network create with name and IPv6 subnet", func() {
		netName := "ipv6-" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{"network", "create", "--subnet", "fd00:1:2:3:4::/64", netName})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName)
		Expect(nc).Should(Exit(0))

		// Inspect the network configuration
		inspect := podmanTest.Podman([]string{"network", "inspect", netName})
		inspect.WaitWithDefaultTimeout()
		Expect(inspect).Should(Exit(0))

		// JSON the network configuration into something usable
		var results []types.Network
		err := json.Unmarshal([]byte(inspect.OutputToString()), &results)
		Expect(err).ToNot(HaveOccurred())
		Expect(results).To(HaveLen(1))
		result := results[0]
		Expect(result).To(HaveField("Name", netName))
		Expect(result.Subnets).To(HaveLen(1))
		Expect(result.Subnets[0].Gateway.String()).To(Equal("fd00:1:2:3::1"))
		Expect(result.Subnets[0].Subnet.String()).To(Equal("fd00:1:2:3::/64"))

		// Once a container executes a new network, the nic will be created. We should clean those up
		// best we can
		defer removeNetworkDevice(result.NetworkInterface)

		try := podmanTest.Podman([]string{"run", "--rm", "--network", netName, ALPINE, "sh", "-c", "ip addr show eth0 |  grep global | awk ' /inet6 / {print $2}'"})
		try.WaitWithDefaultTimeout()
		Expect(try).To(Exit(0))

		_, subnet, err := net.ParseCIDR("fd00:1:2:3:4::/64")
		Expect(err).ToNot(HaveOccurred())
		containerIP, _, err := net.ParseCIDR(try.OutputToString())
		Expect(err).ToNot(HaveOccurred())
		// Ensure that the IP the container got is within the subnet the user asked for
		Expect(subnet.Contains(containerIP)).To(BeTrue(), "subnet contains containerIP")
	})

	It("podman network create with name and IPv6 flag (dual-stack)", func() {
		netName := "dual-" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{"network", "create", "--subnet", "fd00:4:3:2::/64", "--ipv6", netName})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName)
		Expect(nc).Should(Exit(0))

		// Inspect the network configuration
		inspect := podmanTest.Podman([]string{"network", "inspect", netName})
		inspect.WaitWithDefaultTimeout()
		Expect(inspect).Should(Exit(0))

		// JSON the network configuration into something usable
		var results []types.Network
		err := json.Unmarshal([]byte(inspect.OutputToString()), &results)
		Expect(err).ToNot(HaveOccurred())
		Expect(results).To(HaveLen(1))
		result := results[0]
		Expect(result).To(HaveField("Name", netName))
		Expect(result.Subnets).To(HaveLen(2))
		Expect(result.Subnets[0].Subnet.IP).ToNot(BeNil())
		Expect(result.Subnets[1].Subnet.IP).ToNot(BeNil())

		_, subnet11, err := net.ParseCIDR(result.Subnets[0].Subnet.String())
		Expect(err).ToNot(HaveOccurred())
		_, subnet12, err := net.ParseCIDR(result.Subnets[1].Subnet.String())
		Expect(err).ToNot(HaveOccurred())

		// Once a container executes a new network, the nic will be created. We should clean those up
		// best we can
		defer removeNetworkDevice(result.NetworkInterface)

		// create a second network to check the auto assigned ipv4 subnet does not overlap
		// https://github.com/containers/podman/issues/11032
		netName2 := "dual-" + stringid.GenerateRandomID()
		nc = podmanTest.Podman([]string{"network", "create", "--subnet", "fd00:10:3:2::/64", "--ipv6", netName2})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName2)
		Expect(nc).Should(Exit(0))

		// Inspect the network configuration
		inspect = podmanTest.Podman([]string{"network", "inspect", netName2})
		inspect.WaitWithDefaultTimeout()
		Expect(inspect).Should(Exit(0))

		// JSON the network configuration into something usable
		err = json.Unmarshal([]byte(inspect.OutputToString()), &results)
		Expect(err).ToNot(HaveOccurred())
		Expect(results).To(HaveLen(1))
		result = results[0]
		Expect(result).To(HaveField("Name", netName2))
		Expect(result.Subnets).To(HaveLen(2))
		Expect(result.Subnets[0].Subnet.IP).ToNot(BeNil())
		Expect(result.Subnets[1].Subnet.IP).ToNot(BeNil())

		_, subnet21, err := net.ParseCIDR(result.Subnets[0].Subnet.String())
		Expect(err).ToNot(HaveOccurred())
		_, subnet22, err := net.ParseCIDR(result.Subnets[1].Subnet.String())
		Expect(err).ToNot(HaveOccurred())

		// check that the subnets do not overlap
		Expect(subnet11.Contains(subnet21.IP)).To(BeFalse())
		Expect(subnet12.Contains(subnet22.IP)).To(BeFalse())

		try := podmanTest.Podman([]string{"run", "--rm", "--network", netName, ALPINE, "sh", "-c", "ip addr show eth0 |  grep global | awk ' /inet6 / {print $2}'"})
		try.WaitWithDefaultTimeout()

		_, subnet, err := net.ParseCIDR("fd00:4:3:2:1::/64")
		Expect(err).ToNot(HaveOccurred())
		containerIP, _, err := net.ParseCIDR(try.OutputToString())
		Expect(err).ToNot(HaveOccurred())
		// Ensure that the IP the container got is within the subnet the user asked for
		Expect(subnet.Contains(containerIP)).To(BeTrue(), "subnet contains containerIP")
		// verify the container has an IPv4 address too (the IPv4 subnet is autogenerated)
		try = podmanTest.Podman([]string{"run", "--rm", "--network", netName, ALPINE, "sh", "-c", "ip addr show eth0 |  awk ' /inet / {print $2}'"})
		try.WaitWithDefaultTimeout()
		containerIP, _, err = net.ParseCIDR(try.OutputToString())
		Expect(err).ToNot(HaveOccurred())
		Expect(containerIP.To4()).To(Not(BeNil()))
	})

	It("podman network create with invalid subnet", func() {
		nc := podmanTest.Podman([]string{"network", "create", "--subnet", "10.11.12.0/17000", stringid.GenerateRandomID()})
		nc.WaitWithDefaultTimeout()
		Expect(nc).To(ExitWithError())
	})

	It("podman network create with ipv4 subnet and ipv6 flag", func() {
		name := stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{"network", "create", "--subnet", "10.11.14.0/24", "--ipv6", name})
		nc.WaitWithDefaultTimeout()
		Expect(nc).To(Exit(0))
		defer podmanTest.removeNetwork(name)

		nc = podmanTest.Podman([]string{"network", "inspect", name})
		nc.WaitWithDefaultTimeout()
		Expect(nc).To(Exit(0))
		Expect(nc.OutputToString()).To(ContainSubstring(`::/64`))
		Expect(nc.OutputToString()).To(ContainSubstring(`10.11.14.0/24`))
	})

	It("podman network create with empty subnet and ipv6 flag", func() {
		name := stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{"network", "create", "--ipv6", name})
		nc.WaitWithDefaultTimeout()
		Expect(nc).To(Exit(0))
		defer podmanTest.removeNetwork(name)

		nc = podmanTest.Podman([]string{"network", "inspect", name})
		nc.WaitWithDefaultTimeout()
		Expect(nc).To(Exit(0))
		Expect(nc.OutputToString()).To(ContainSubstring(`::/64`))
		Expect(nc.OutputToString()).To(ContainSubstring(`.0/24`))
	})

	It("podman network create with invalid IP", func() {
		nc := podmanTest.Podman([]string{"network", "create", "--subnet", "10.11.0/17000", stringid.GenerateRandomID()})
		nc.WaitWithDefaultTimeout()
		Expect(nc).To(ExitWithError())
	})

	It("podman network create with invalid gateway for subnet", func() {
		nc := podmanTest.Podman([]string{"network", "create", "--subnet", "10.11.12.0/24", "--gateway", "192.168.1.1", stringid.GenerateRandomID()})
		nc.WaitWithDefaultTimeout()
		Expect(nc).To(ExitWithError())
	})

	It("podman network create two networks with same name should fail", func() {
		netName := "same-" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{"network", "create", netName})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName)
		Expect(nc).Should(Exit(0))

		ncFail := podmanTest.Podman([]string{"network", "create", netName})
		ncFail.WaitWithDefaultTimeout()
		Expect(ncFail).To(ExitWithError())
	})

	It("podman network create two networks with same subnet should fail", func() {
		netName1 := "sub1-" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{"network", "create", "--subnet", "10.11.13.0/24", netName1})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName1)
		Expect(nc).Should(Exit(0))

		netName2 := "sub2-" + stringid.GenerateRandomID()
		ncFail := podmanTest.Podman([]string{"network", "create", "--subnet", "10.11.13.0/24", netName2})
		ncFail.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName2)
		Expect(ncFail).To(ExitWithError())
	})

	It("podman network create two IPv6 networks with same subnet should fail", func() {
		netName1 := "subipv61-" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{"network", "create", "--subnet", "fd00:4:4:4:4::/64", "--ipv6", netName1})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName1)
		Expect(nc).Should(Exit(0))

		netName2 := "subipv62-" + stringid.GenerateRandomID()
		ncFail := podmanTest.Podman([]string{"network", "create", "--subnet", "fd00:4:4:4:4::/64", "--ipv6", netName2})
		ncFail.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName2)
		Expect(ncFail).To(ExitWithError())
	})

	It("podman network create with invalid network name", func() {
		nc := podmanTest.Podman([]string{"network", "create", "foo "})
		nc.WaitWithDefaultTimeout()
		Expect(nc).To(ExitWithError())
	})

	It("podman network create with mtu option", func() {
		net := "mtu-test" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{"network", "create", "--opt", "mtu=9000", net})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(net)
		Expect(nc).Should(Exit(0))

		nc = podmanTest.Podman([]string{"network", "inspect", net})
		nc.WaitWithDefaultTimeout()
		Expect(nc).Should(Exit(0))
		Expect(nc.OutputToString()).To(ContainSubstring(`"mtu": "9000"`))
	})

	It("podman network create with vlan option", func() {
		net := "vlan-test" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{"network", "create", "--opt", "vlan=9", net})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(net)
		Expect(nc).Should(Exit(0))

		nc = podmanTest.Podman([]string{"network", "inspect", net})
		nc.WaitWithDefaultTimeout()
		Expect(nc).Should(Exit(0))
		Expect(nc.OutputToString()).To(ContainSubstring(`"vlan": "9"`))
	})

	It("podman network create with invalid option", func() {
		net := "invalid-test" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{"network", "create", "--opt", "foo=bar", net})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(net)
		Expect(nc).To(ExitWithError())
	})

	It("podman CNI network create with internal should not have dnsname", func() {
		SkipIfNetavark(podmanTest)
		net := "internal-test" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{"network", "create", "--internal", net})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(net)
		Expect(nc).Should(Exit(0))
		// Not performing this check on remote tests because it is a logrus error which does
		// not come back via stderr on the remote client.
		if !IsRemote() {
			Expect(nc.ErrorToString()).To(ContainSubstring("dnsname and internal networks are incompatible"))
		}
		nc = podmanTest.Podman([]string{"network", "inspect", net})
		nc.WaitWithDefaultTimeout()
		Expect(nc).Should(Exit(0))
		Expect(nc.OutputToString()).ToNot(ContainSubstring("dnsname"))
	})

	It("podman Netavark network create with internal should have dnsname", func() {
		SkipIfCNI(podmanTest)
		net := "internal-test" + stringid.GenerateRandomID()
		nc := podmanTest.Podman([]string{"network", "create", "--internal", net})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(net)
		Expect(nc).Should(Exit(0))
		// Not performing this check on remote tests because it is a logrus error which does
		// not come back via stderr on the remote client.
		if !IsRemote() {
			Expect(nc.ErrorToString()).To(BeEmpty())
		}
		nc = podmanTest.Podman([]string{"network", "inspect", net})
		nc.WaitWithDefaultTimeout()
		Expect(nc).Should(Exit(0))
		Expect(nc.OutputToString()).To(ContainSubstring(`"dns_enabled": true`))
	})

	It("podman network create with invalid name", func() {
		for _, name := range []string{"none", "host", "bridge", "private", "slirp4netns", "container", "ns", "default"} {
			nc := podmanTest.Podman([]string{"network", "create", name})
			nc.WaitWithDefaultTimeout()
			Expect(nc).To(Exit(125))
			Expect(nc.ErrorToString()).To(ContainSubstring("cannot create network with name %q because it conflicts with a valid network mode", name))
		}
	})

	It("podman network create with multiple subnets", func() {
		name := "subnets-" + stringid.GenerateRandomID()
		subnet1 := "10.10.0.0/24"
		subnet2 := "10.10.1.0/24"
		nc := podmanTest.Podman([]string{"network", "create", "--subnet", subnet1, "--subnet", subnet2, name})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(name)
		Expect(nc).To(Exit(0))
		Expect(nc.OutputToString()).To(Equal(name))

		inspect := podmanTest.Podman([]string{"network", "inspect", name})
		inspect.WaitWithDefaultTimeout()
		Expect(inspect).To(Exit(0))
		Expect(inspect.OutputToString()).To(ContainSubstring(`"subnet": "` + subnet1))
		Expect(inspect.OutputToString()).To(ContainSubstring(`"subnet": "` + subnet2))
		Expect(inspect.OutputToString()).To(ContainSubstring(`"ipv6_enabled": false`))
	})

	It("podman network create with multiple subnets dual stack", func() {
		name := "subnets-" + stringid.GenerateRandomID()
		subnet1 := "10.10.2.0/24"
		subnet2 := "fd52:2a5a:747e:3acd::/64"
		nc := podmanTest.Podman([]string{"network", "create", "--subnet", subnet1, "--subnet", subnet2, name})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(name)
		Expect(nc).To(Exit(0))
		Expect(nc.OutputToString()).To(Equal(name))

		inspect := podmanTest.Podman([]string{"network", "inspect", name})
		inspect.WaitWithDefaultTimeout()
		Expect(inspect).To(Exit(0))
		Expect(inspect.OutputToString()).To(ContainSubstring(`"subnet": "` + subnet1))
		Expect(inspect.OutputToString()).To(ContainSubstring(`"subnet": "` + subnet2))
		Expect(inspect.OutputToString()).To(ContainSubstring(`"ipv6_enabled": true`))
	})

	It("podman network create with multiple subnets dual stack with gateway and range", func() {
		name := "subnets-" + stringid.GenerateRandomID()
		subnet1 := "10.10.3.0/24"
		gw1 := "10.10.3.10"
		range1 := "10.10.3.0/26"
		subnet2 := "fd52:2a5a:747e:3ace::/64"
		gw2 := "fd52:2a5a:747e:3ace::10"
		nc := podmanTest.Podman([]string{"network", "create", "--subnet", subnet1, "--gateway", gw1, "--ip-range", range1, "--subnet", subnet2, "--gateway", gw2, name})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(name)
		Expect(nc).To(Exit(0))
		Expect(nc.OutputToString()).To(Equal(name))

		inspect := podmanTest.Podman([]string{"network", "inspect", name})
		inspect.WaitWithDefaultTimeout()
		Expect(inspect).To(Exit(0))
		Expect(inspect.OutputToString()).To(ContainSubstring(`"subnet": "` + subnet1))
		Expect(inspect.OutputToString()).To(ContainSubstring(`"gateway": "` + gw1))
		Expect(inspect.OutputToString()).To(ContainSubstring(`"start_ip": "10.10.3.1",`))
		Expect(inspect.OutputToString()).To(ContainSubstring(`"end_ip": "10.10.3.63"`))
		Expect(inspect.OutputToString()).To(ContainSubstring(`"subnet": "` + subnet2))
		Expect(inspect.OutputToString()).To(ContainSubstring(`"gateway": "` + gw2))
		Expect(inspect.OutputToString()).To(ContainSubstring(`"ipv6_enabled": true`))
	})

	It("podman network create invalid options with multiple subnets", func() {
		name := "subnets-" + stringid.GenerateRandomID()
		subnet1 := "10.10.3.0/24"
		gw1 := "10.10.3.10"
		gw2 := "fd52:2a5a:747e:3acf::10"
		nc := podmanTest.Podman([]string{"network", "create", "--subnet", subnet1, "--gateway", gw1, "--gateway", gw2, name})
		nc.WaitWithDefaultTimeout()
		Expect(nc).To(Exit(125))
		Expect(nc.ErrorToString()).To(Equal("Error: cannot set more gateways than subnets"))

		range1 := "10.10.3.0/26"
		range2 := "10.10.3.0/28"
		nc = podmanTest.Podman([]string{"network", "create", "--subnet", subnet1, "--ip-range", range1, "--ip-range", range2, name})
		nc.WaitWithDefaultTimeout()
		Expect(nc).To(Exit(125))
		Expect(nc.ErrorToString()).To(Equal("Error: cannot set more ranges than subnets"))
	})

	It("podman network create same name - fail", func() {
		name := "same-name-" + stringid.GenerateRandomID()
		networkCreateCommand := []string{"network", "create", name}
		nc := podmanTest.Podman(networkCreateCommand)
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(name)
		Expect(nc).To(Exit(0))
		Expect(nc.OutputToString()).To(Equal(name))

		nc = podmanTest.Podman(networkCreateCommand)
		nc.WaitWithDefaultTimeout()
		Expect(nc).To(Exit(125))
	})

	It("podman network create same name - succeed with ignore", func() {
		name := "same-name-" + stringid.GenerateRandomID()
		networkCreateCommand := []string{"network", "create", "--ignore", name}
		nc := podmanTest.Podman(networkCreateCommand)
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(name)
		Expect(nc).To(Exit(0))
		Expect(nc.OutputToString()).To(Equal(name))

		nc = podmanTest.Podman(networkCreateCommand)
		nc.WaitWithDefaultTimeout()
		Expect(nc).To(Exit(0))
		Expect(nc.OutputToString()).To(Equal(name))
	})

	It("podman network create --interface-name", func() {
		netName := "bridge-" + stringid.GenerateRandomID()
		bridgeName := "mybridge" + stringid.GenerateRandomID()[:4]
		nc := podmanTest.Podman([]string{"network", "create", "--interface-name", bridgeName, netName})
		nc.WaitWithDefaultTimeout()
		defer podmanTest.removeNetwork(netName)
		Expect(nc).To(Exit(0))
		Expect(nc.OutputToString()).To(Equal(netName))

		session := podmanTest.Podman([]string{"network", "inspect", "--format", "{{.NetworkInterface}}", netName})
		session.WaitWithDefaultTimeout()
		Expect(session).To(Exit(0))
		Expect(session.OutputToString()).To(Equal(bridgeName))

		session = podmanTest.Podman([]string{"run", "-d", "--network", netName, ALPINE, "top"})
		session.WaitWithDefaultTimeout()
		Expect(session).To(Exit(0))

		// can only check this as root
		if !isRootless() {
			// make sure cni/netavark created bridge with expected name
			bridge, err := net.InterfaceByName(bridgeName)
			Expect(err).ToNot(HaveOccurred())
			Expect(bridge.Name).To(Equal(bridgeName))
		}
	})
})
