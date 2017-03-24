package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

var prometheusContainerID = "b171793f9e26"

// allocateIP returns the 3rd last IP in the network range.
func allocateIP(netCIDR *net.IPNet) string {

	allocIP := net.IP(make([]byte, 4))
	for i := range netCIDR.IP {
		allocIP[i] = netCIDR.IP[i] | ^netCIDR.Mask[i]
	}

	allocIP[3] = allocIP[3] - 2
	return allocIP.String()
}

func connectNetworks(networks map[string]swarm.Network) {
	cli, err := client.NewEnvClient()
	if err != nil {
		panic(err)
	}

	prometheusContainer, err := cli.ContainerInspect(context.Background(), prometheusContainerID)
	if err != nil {
		panic(err)
	}

	promNetworks := make(map[string]*network.EndpointSettings)

	for _, cntnet := range prometheusContainer.NetworkSettings.Networks {
		promNetworks[cntnet.NetworkID] = cntnet
	}

	for id, netwrk := range networks {
		if _, ok := promNetworks[id]; ok || len(netwrk.IPAMOptions.Configs) == 0 {
			continue
		}

		_, netCIDR, err := net.ParseCIDR(netwrk.IPAMOptions.Configs[0].Subnet)
		if err != nil {
			panic(err)
		}
		prometheusIP := allocateIP(netCIDR)
		fmt.Println("Connecting network ", netwrk.Spec.Name, "(", netCIDR.IP, ") to ", prometheusContainerID, "(", prometheusIP, ")")
		netconfig := &network.EndpointSettings{
			IPAMConfig: &network.EndpointIPAMConfig{
				IPv4Address: prometheusIP,
			},
		}

		err = cli.NetworkConnect(context.Background(), netwrk.ID, prometheusContainerID, netconfig)
		if err != nil {
			panic(err)
		}

	}

}

func writeSDConfig(scrapeTargets []scrapeTarget) {
	jsonScrapeConfig, err := json.MarshalIndent(scrapeTargets, "", "  ")
	if err != nil {
		panic(err)
	}

	fmt.Println("Writing config file.")

	err = ioutil.WriteFile("swarm-endpoints.json", jsonScrapeConfig, 0644)
	if err != nil {
		panic(err)
	}
}

type scrapeTarget struct {
	Targets []string
	Labels  map[string]string
}

func discoverSwarm(cmd *cobra.Command, args []string) {
	cli, err := client.NewEnvClient()
	if err != nil {
		panic(err)
	}

	taskFilters := filters.NewArgs()
	taskFilters.Add("desired-state", string(swarm.TaskStateRunning))

	tasks, err := cli.TaskList(context.Background(), types.TaskListOptions{Filters: taskFilters})
	if err != nil {
		panic(err)
	}

	var scrapeTargets []scrapeTarget
	taskNetworks := make(map[string]swarm.Network)

	for _, task := range tasks {
		taskRaw, _, err := cli.TaskInspectWithRaw(context.Background(), task.ID)
		if err != nil {
			panic(err)
		}

		var containerIPs []string

		for _, netatt := range taskRaw.NetworksAttachments {
			if netatt.Network.Spec.Name == "ingress" || netatt.Network.DriverState.Name != "overlay" {
				continue
			}

			for _, ipcidr := range netatt.Addresses {
				ip, _, err := net.ParseCIDR(ipcidr)
				if err != nil {
					panic(err)
				}
				containerIPs = append(containerIPs, ip.String())
			}

			fmt.Printf("Task %s %s\n", task.ID, containerIPs)
			taskNetworks[netatt.Network.ID] = netatt.Network
			scrapeTargets = append(scrapeTargets, scrapeTarget{Targets: containerIPs, Labels: map[string]string{"labelname": "value"}})
		}
	}

	connectNetworks(taskNetworks)
	writeSDConfig(scrapeTargets)
}

func main() {

	var cmdDiscover = &cobra.Command{
		Use:   "discover",
		Short: "Starts Swarm service discovery",
		Run:   discoverSwarm,
	}

	var rootCmd = &cobra.Command{Use: "promswarm"}
	rootCmd.AddCommand(cmdDiscover)
	rootCmd.Execute()

}
