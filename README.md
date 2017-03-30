# Prometheus-Swarm service discovery

This is POC that demonstrates Prometheus service discovery in Docker Swarm. It is implemented as a standalone tool
that writes the scrape targets to a file, that is then read by Prometheus. This uses the `<file_sd_config>` config
directive available in Prometheus.

The discovery tool expects to have access to the Swarm API using `/var/run/docker.sock`. The provided `docker-compose.yaml`
is configured to mount the `docker.sock` file inside the container, and this requires that the `swarm-discover` container
is placed on the Swarm manager node.

## Run Prometheus and the Swarm discovery tool

The only thing required to run Prometheus and the discovery tool is to launch a Swarm stack using the provided docker-compose.yaml
file.

```
$ docker stack deploy -c docker-compose.yaml prometheus
```
