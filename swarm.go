package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"reflect"
	"strconv"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/util/strutil"
	"github.com/spf13/cobra"
)

const (
	implicit     = "implicit"
	explicit     = "explicit"
	portLabel    = "prometheus.port"
	includeLabel = "prometheus.scan"
	excludeLabel = "prometheus.ignore"
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

func connectNetworks(networks map[string]swarm.Network, containerID string) error {
	cli, err := client.NewEnvClient()
	if err != nil {
		return err
	}

	prometheusContainer, err := cli.ContainerInspect(context.Background(), containerID)
	if err != nil {
		return err
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
			logger.Error(err)
			continue
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
			logger.Error("Could not connect container ", containerID, " to network ", netwrk.ID, ": ", err)
			continue
		}

	}

	return nil

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

	if len(promTasks) == 0 || promTasks[0].Status.ContainerStatus.ContainerID == "" {
		return "", fmt.Errorf("Could not find container for service %s", serviceName)
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

// collectPorts builds a map of ports collected from container exposed ports and/or from ports defined
// as container labels
func collectPorts(task swarm.Task, serviceIDMap map[string]swarm.Service, discoveryType string) map[int]struct{} {

	ports := make(map[int]struct{})

	// collects port defined in the container labels
	if portstr, ok := task.Spec.ContainerSpec.Labels[portLabel]; ok {
		if port, err := strconv.Atoi(portstr); err == nil {
			ports[port] = struct{}{}
		}
	}

	// collects exposed ports, but only if we use implicit discovery
	if discoveryType == implicit {
		for _, port := range serviceIDMap[task.ServiceID].Spec.EndpointSpec.Ports {
			ports[int(port.TargetPort)] = struct{}{}
		}
	} else {
		logger.Debugf("Exposed ports on Service %s ignored by Prometheus", task.ServiceID)
	}

	return ports
}

func collectIPs(task swarm.Task) ([]net.IP, map[string]swarm.Network) {

	var containerIPs []net.IP
	taskNetworks := make(map[string]swarm.Network)

	for _, netatt := range task.NetworksAttachments {
		if netatt.Network.Spec.Name == "ingress" || netatt.Network.DriverState.Name != "overlay" {
			continue
		}

		for _, ipcidr := range netatt.Addresses {
			ip, _, err := net.ParseCIDR(ipcidr)
			if err != nil {
				logger.Error(err)
				continue
			}

			containerIPs = append(containerIPs, ip)
		}
		taskNetworks[netatt.Network.ID] = netatt.Network
	}

	return containerIPs, taskNetworks
}

func taskLabels(task swarm.Task, serviceIDMap map[string]swarm.Service) map[string]string {
	service := serviceIDMap[task.ServiceID]
	labels := map[string]string{
		model.JobLabel: service.Spec.Name,

		model.MetaLabelPrefix + "docker_task_name":          task.Name,
		model.MetaLabelPrefix + "docker_task_desired_state": string(task.DesiredState),
	}
	for k, v := range task.Labels {
		labels[strutil.SanitizeLabelName(model.MetaLabelPrefix+"docker_task_label_"+k)] = v
	}
	for k, v := range service.Spec.Labels {
		labels[strutil.SanitizeLabelName(model.MetaLabelPrefix+"docker_service_label_"+k)] = v
	}
	return labels
}

func discoverSwarm(prometheusContainerID string, outputFile string, discoveryType string) {
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
	allNetworks := make(map[string]swarm.Network)

	for _, task := range tasks {

		if discoveryType == implicit {
			if _, ok := task.Spec.ContainerSpec.Labels[excludeLabel]; ok {
				logger.Debugf("Task %s ignored by Prometheus", task.ID)
				continue
			}
		} else if discoveryType == explicit {
			if _, ok := task.Spec.ContainerSpec.Labels[includeLabel]; ok {
				logger.Debugf("Task %s should be scanned by Prometheus", task.ID)
			} else {
				continue
			}
		}

		ports := collectPorts(task, serviceIDMap, discoveryType)
		containerIPs, taskNetworks := collectIPs(task)
		var taskEndpoints []string

		for k, v := range taskNetworks {
			allNetworks[k] = v
		}

		// if exposed ports are found, or ports defined through labels, add them to the Prometheus target.
		// if not, add only the container IP as a target, and Prometheus will use the default port (80).
		for _, ip := range containerIPs {
			if len(ports) > 0 {
				for port := range ports {
					taskEndpoints = append(taskEndpoints, fmt.Sprintf("%s:%d", ip.String(), port))
				}
			} else {
				taskEndpoints = append(taskEndpoints, ip.String())
			}
		}

		logger.Debugf("Found task %s with IPs %s", task.ID, taskEndpoints)

		scrapetask := scrapeTask{
			Targets: taskEndpoints,
			Labels:  taskLabels(task, serviceIDMap),
		}

		scrapeTasks = append(scrapeTasks, scrapetask)
	}

	err = connectNetworks(allNetworks, prometheusContainerID)
	if err != nil {
		logger.Error("Could not connect container ,", prometheusContainerID, ": ", err)
	}
	writeSDConfig(scrapeTasks, outputFile)
}

func discoveryProcess(cmd *cobra.Command, args []string) {

	level, err := logrus.ParseLevel(options.logLevel)
	if err != nil {
		logger.Fatal(err)
	}
	logger.Level = level

	if options.discovery != implicit && options.discovery != explicit {
		logger.Fatal("Invalid discovery type: ", options.discovery)
	}

	logger.Info("Starting service discovery process using Prometheus service [", options.prometheusService, "]")

	for {
		time.Sleep(time.Duration(options.discoveryInterval) * time.Second)
		prometheusContainerID, err := findPrometheusContainer(options.prometheusService)
		if err != nil {
			logger.Warn(err)
			continue
		}

		discoverSwarm(prometheusContainerID, options.output, options.discovery)
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
