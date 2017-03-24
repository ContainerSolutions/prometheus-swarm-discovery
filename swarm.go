package main

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"net"
	"time"

	"strings"

	"fmt"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

var prometheusService string
var discoveryInterval int
var logLevel string
var logger = logrus.New()

// allocateIP returns the 3rd last IP in the network range.
func allocateIP(netCIDR *net.IPNet) string {

	allocIP := net.IP(make([]byte, 4))
	for i := range netCIDR.IP {
		allocIP[i] = netCIDR.IP[i] | ^netCIDR.Mask[i]
	}

	allocIP[3] = allocIP[3] - 2
	return allocIP.String()
}

func connectNetworks(networks map[string]swarm.Network, containerID string) {
	cli, err := client.NewEnvClient()
	if err != nil {
		panic(err)
	}

	prometheusContainer, err := cli.ContainerInspect(context.Background(), containerID)
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
		logger.Info("Connecting network ", netwrk.Spec.Name, "(", netCIDR.IP, ") to ", containerID, "(", prometheusIP, ")")
		netconfig := &network.EndpointSettings{
			IPAMConfig: &network.EndpointIPAMConfig{
				IPv4Address: prometheusIP,
			},
		}

		err = cli.NetworkConnect(context.Background(), netwrk.ID, containerID, netconfig)
		if err != nil {
			panic(err)
		}

	}

}

func writeSDConfig(scrapeTasks []scrapeTask) {
	jsonScrapeConfig, err := json.MarshalIndent(scrapeTasks, "", "  ")
	if err != nil {
		panic(err)
	}

	logger.Debug("Writing Prometheus config file")

	err = ioutil.WriteFile("swarm-endpoints.json", jsonScrapeConfig, 0644)
	if err != nil {
		panic(err)
	}
}

func findPrometheusContainer(serviceName string) (string, error) {
	cli, err := client.NewEnvClient()
	if err != nil {
		panic(err)
	}

	taskFilters := filters.NewArgs()
	taskFilters.Add("desired-state", string(swarm.TaskStateRunning))
	taskFilters.Add("service", serviceName)

	promTasks, err := cli.TaskList(context.Background(), types.TaskListOptions{Filters: taskFilters})
	if err != nil {
		return "", err
	}

	return promTasks[0].Status.ContainerStatus.ContainerID, nil
}

type scrapeService struct {
	ServiceName string
	scrapeTasks scrapeTask
}

type scrapeTask struct {
	Targets []string
	Labels  map[string]string
}

func discoverSwarm(prometheusContainerID string) {
	cli, err := client.NewEnvClient()
	if err != nil {
		panic(err)
	}

	services, err := cli.ServiceList(context.Background(), types.ServiceListOptions{})
	if err != nil {
		panic(err)
	}
	serviceIDMap := make(map[string]swarm.Service)
	for _, service := range services {
		serviceIDMap[service.ID] = service
	}

	taskFilters := filters.NewArgs()
	taskFilters.Add("desired-state", string(swarm.TaskStateRunning))
	tasks, err := cli.TaskList(context.Background(), types.TaskListOptions{Filters: taskFilters})
	if err != nil {
		panic(err)
	}

	var scrapeTasks []scrapeTask
	taskNetworks := make(map[string]swarm.Network)

	for _, task := range tasks {

		var containerIPs []string

		for _, netatt := range task.NetworksAttachments {
			if netatt.Network.Spec.Name == "ingress" || netatt.Network.DriverState.Name != "overlay" {
				continue
			}

			for _, ipcidr := range netatt.Addresses {
				ip, _, err := net.ParseCIDR(ipcidr)
				if err != nil {
					panic(err)
				}
				if len(serviceIDMap[task.ServiceID].Spec.EndpointSpec.Ports) > 0 {
					for _, port := range serviceIDMap[task.ServiceID].Spec.EndpointSpec.Ports {
						containerIPs = append(containerIPs, fmt.Sprintf("%s:%d", ip.String(), port.TargetPort))
					}
				} else {
					containerIPs = append(containerIPs, ip.String())
				}
			}

			logger.Debugf("Found task %s with IPs %s", task.ID, containerIPs)
			taskNetworks[netatt.Network.ID] = netatt.Network
			scrapetask := scrapeTask{
				Targets: containerIPs,
				Labels: map[string]string{
					"job": serviceIDMap[task.ServiceID].Spec.Name,
				},
			}
			scrapeTasks = append(scrapeTasks, scrapetask)
		}
	}

	connectNetworks(taskNetworks, prometheusContainerID)
	writeSDConfig(scrapeTasks)
}

func discoveryProcess(cmd *cobra.Command, args []string) {

	if logLevel == "debug" {
		logger.Level = logrus.DebugLevel
	} else if logLevel == "info" {
		logger.Level = logrus.InfoLevel
	} else if logLevel == "warn" {
		logger.Level = logrus.WarnLevel
	} else if logLevel == "error" {
		logger.Level = logrus.ErrorLevel
	} else {
		logger.Fatal("Invalid log level: ", logLevel)
	}

	logger.Info("Starting service discovery process using Prometheus service [", prometheusService, "]")

	for {
		time.Sleep(time.Duration(discoveryInterval) * time.Second)
		prometheusContainerID, err := findPrometheusContainer(prometheusService)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				logger.Warn("Service ", prometheusService, " not running yet.")
				continue
			}
			logger.Fatal(err)
		}

		discoverSwarm(prometheusContainerID)
	}
}

func main() {

	var cmdDiscover = &cobra.Command{
		Use:   "discover",
		Short: "Starts Swarm service discovery",
		Run:   discoveryProcess,
	}

	cmdDiscover.Flags().StringVarP(&prometheusService, "prometheus", "p", "prometheus", "Name of the Prometheus service")
	cmdDiscover.Flags().IntVarP(&discoveryInterval, "interval", "i", 30, "The interval, in seconds, at which the discovery process is kicked off")
	cmdDiscover.Flags().StringVarP(&logLevel, "loglevel", "l", "info", "Specify log level: debug, info, warn, error")

	var rootCmd = &cobra.Command{Use: "promswarm"}
	rootCmd.AddCommand(cmdDiscover)
	rootCmd.Execute()

}
