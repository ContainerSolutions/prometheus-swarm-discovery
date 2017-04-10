# Prometheus-Swarm service discovery

This is a POC that demonstrates Prometheus service discovery in Docker Swarm. At the moment, this POC only discovers Swarm services and their respective tasks, without attempting to discover nodes or other Swarm concepts.

## How it works

It is implemented as a standalone tool that writes the scrape targets to a file, that is then read by Prometheus. This uses the `<file_sd_config>` config
directive available in Prometheus.

The discovery tool expects to have access to the Swarm API using `/var/run/docker.sock`. The provided `docker-compose.yaml`
is configured to mount the `docker.sock` file inside the container, and this requires that the `swarm-discover` container
is placed on the Swarm manager node.

The discovery loop has 3 steps:
* read the Swarm API and collect all the services (and their tasks) + the networks they are connected to
* write the scrape targets in the configuration file (`<file_sd_config>`)
* connect the Prometheus container to all the networks that belong to the discovered services.

The rest is done by Prometheus. Whenever the scrape target configuration file is updated, Prometheus re-reads it and loads the current scrape targets.

## Running it

The only thing required to run Prometheus and the discovery tool is to launch a Swarm stack using the provided docker-compose.yaml
file.

```
$ docker stack deploy -c docker-compose.yaml prometheus
```

## Port discovery

By default, Prometheus will use port 80 to connect to a scrape target. But there are 2 ways in which a scrape target can have a different port configured:

* have a port exposed in Docker. That way, the discovery tool can figure out on its own which port to configure for the scrape target.
* annotate a service using Docker labels. A label with format `prometheus.port: <port_nr>` can be added to each service that requires a custom port configuration.

As an example here is the following Docker Compose service definition that uses this feature:

```
version: '3'

services:
  front-end:
    image: weaveworksdemos/front-end
    labels:
        prometheus.port: 8079
```

## Metadata labels

The discovery tool attaches a set of metadata labels to each target that are available during the [relabeling phase](https://prometheus.io/docs/operating/configuration/#<relabel_config>) of the service discovery in Prometheus:

* `__meta_docker_service_label_<labelname>`: The value of this service label.
* `__meta_docker_task_label_<labelname>`: The value of this task label.
* `__meta_docker_task_name`: The name of the Docker task.

Labels starting with `__` are removed after the relabeling phase, so that these labels will not show up on time series directly.

## Excluding services

To exclude a specific service from being included in the scrape targets, add a label of format `prometheus.ignore: "true"`.

Example Docker Compose service:

```
version: '3'

services:
  front-end:
    image: weaveworksdemos/front-end
    labels:
        prometheus.ignore: "true"
```

## Configuration options

```
$ ./prometheus-swarm discover --help
Starts Swarm service discovery

Usage:
  promswarm discover [flags]

Flags:
  -c, --clean               Disconnects unused networks from the Prometheus container, and deletes them. (default true)
  -i, --interval int        The interval, in seconds, at which the discovery process is kicked off (default 30)
  -l, --loglevel string     Specify log level: debug, info, warn, error (default "info")
  -o, --output string       Output file that contains the Prometheus endpoints. (default "swarm-endpoints.json")
  -p, --prometheus string   Name of the Prometheus service (default "prometheus")
```
