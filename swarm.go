package main

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"net"
	"reflect"
	"strconv"
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

var logger = logrus.New()
var options = Options{}

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

func writeSDConfig(scrapeTasks []scrapeTask, output string) {
	jsonScrapeConfig, err := json.MarshalIndent(scrapeTasks, "", "  ")
	if err != nil {
		panic(err)
	}

	logger.Debug("Writing Prometheus config file")

	err = ioutil.WriteFile(output, jsonScrapeConfig, 0644)
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

func cleanNetworks(prometheusContainerID string) {
	cli, err := client.NewEnvClient()

	networkFilters := filters.NewArgs()
	networkFilters.Add("driver", "overlay")
	networks, err := cli.NetworkList(context.Background(), types.NetworkListOptions{Filters: networkFilters})
	if err != nil {
		panic(err)
	}

	for _, network := range networks {
		if network.Name == "ingress" {
			continue
		}

		if len(network.Containers) == 1 && reflect.ValueOf(network.Containers).MapKeys()[0].String() == prometheusContainerID {
			logger.Info("Network ", network.Name, " contains only the Prometheus container. Disconnecting and removing network.")
			cli.NetworkDisconnect(context.Background(), network.ID, prometheusContainerID, true)
			if err != nil {
				logger.Error(err)
			}
			err := cli.NetworkRemove(context.Background(), network.ID)
			if err != nil {
				logger.Error(err)
			}
		}
	}

}

type scrapeService struct {
	ServiceName string
	scrapeTasks scrapeTask
}

type scrapeTask struct {
	Targets []string
	Labels  map[string]string
}

func discoverSwarm(prometheusContainerID string, outputFile string) {
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

		if _, ok := task.Spec.ContainerSpec.Labels["prometheus.ignore"]; ok {
			logger.Debugf("Task %s ignored by Prometheus", task.ID)
			continue
		}

		for _, netatt := range task.NetworksAttachments {
			if netatt.Network.Spec.Name == "ingress" || netatt.Network.DriverState.Name != "overlay" {
				continue
			}

			for _, ipcidr := range netatt.Addresses {
				ip, _, err := net.ParseCIDR(ipcidr)
				if err != nil {
					panic(err)
				}

				ports := make(map[int]struct{})

				if portstr, ok := task.Spec.ContainerSpec.Labels["prometheus.port"]; ok {
					if port, err := strconv.Atoi(portstr); err == nil {
						ports[port] = struct{}{}
					}
				}

				for _, port := range serviceIDMap[task.ServiceID].Spec.EndpointSpec.Ports {
					ports[int(port.TargetPort)] = struct{}{}
				}

				if len(ports) > 0 {
					for port := range ports {
						logger.Debug("Adding ports ", ports)
						containerIPs = append(containerIPs, fmt.Sprintf("%s:%d", ip.String(), port))
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
	writeSDConfig(scrapeTasks, outputFile)
}

func discoveryProcess(cmd *cobra.Command, args []string) {

	switch level := options.logLevel; level {
	case "debug":
		logger.Level = logrus.DebugLevel
	case "info":
		logger.Level = logrus.InfoLevel
	case "warn":
		logger.Level = logrus.WarnLevel
	case "error":
		logger.Level = logrus.ErrorLevel
	default:
		logger.Fatal("Invalid log level: ", options.logLevel)
	}

	logger.Info("Starting service discovery process using Prometheus service [", options.prometheusService, "]")

	for {
		time.Sleep(time.Duration(options.discoveryInterval) * time.Second)
		prometheusContainerID, err := findPrometheusContainer(options.prometheusService)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				logger.Warn("Service ", options.prometheusService, " not running yet.")
				continue
			}
			logger.Fatal(err)
		}

		discoverSwarm(prometheusContainerID, options.output)
		if options.clean {
			cleanNetworks(prometheusContainerID)
		}
	}
}

// Options structure for all the cmd line flags
type Options struct {
	prometheusService string
	discoveryInterval int
	discovery         string
	logLevel          string
	output            string
	clean             bool
}

func main() {

	var cmdDiscover = &cobra.Command{
		Use:   "discover",
		Short: "Starts Swarm service discovery",
		Run:   discoveryProcess,
	}

	cmdDiscover.Flags().StringVarP(&options.prometheusService, "prometheus", "p", "prometheus", "Name of the Prometheus service")
	cmdDiscover.Flags().IntVarP(&options.discoveryInterval, "interval", "i", 30, "The interval, in seconds, at which the discovery process is kicked off")
	cmdDiscover.Flags().StringVarP(&options.logLevel, "loglevel", "l", "info", "Specify log level: debug, info, warn, error")
	cmdDiscover.Flags().StringVarP(&options.output, "output", "o", "swarm-endpoints.json", "Output file that contains the Prometheus endpoints.")
	cmdDiscover.Flags().BoolVarP(&options.clean, "clean", "c", true, "Disconnects unused networks from the Prometheus container, and deletes them.")
	cmdDiscover.Flags().StringVarP(&options.discovery, "discovery", "d", "implicit", "Discovery time: implicit or explicit. Implicit scans all the services found while explicit scans only labeled services.")

	var rootCmd = &cobra.Command{Use: "promswarm"}
	rootCmd.AddCommand(cmdDiscover)
	rootCmd.Execute()

}
