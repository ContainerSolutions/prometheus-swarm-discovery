# Prometheus-Swarm service discovery

This is POC that demonstrates Prometheus service discovery in Docker Swarm.

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
