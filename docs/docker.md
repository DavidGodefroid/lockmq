# lockmq and containers

## A broker in a container

A RabbitMQ in Docker is a broker like any other: point the profile at the
ports the container publishes, on `127.0.0.1` for a local one.

```toml
[profiles.dev]
url        = "http://127.0.0.1:15672"
amqp_url   = "amqp://127.0.0.1:5672"
stream_url = "rabbitmq-stream://localhost:5552"
```

The stream protocol connects to the address the broker **advertises**, not
the one in `stream_url`: set `stream.advertised_host` and
`stream.advertised_port` in the broker's configuration to what the console
can reach (`localhost` and the published port), or leave `stream_url` out
and read streams over AMQP.

Create the read-only user once, with the container's admin:

```sh
docker exec rabbitmq rabbitmqctl add_user lockmq-ro 'choose-a-password'
docker exec rabbitmq rabbitmqctl set_user_tags lockmq-ro monitoring
docker exec rabbitmq rabbitmqctl set_permissions -p / lockmq-ro '^$' '^$' '^(order-events)$'
```

The last argument is the `read` permission: list the streams lockmq may
read (and the queues, only for a profile with tier `peek`; `read` also
allows a purge, see the security model).

Never give the agent `docker exec` on the broker container: it is a shell
with the broker's own rights.

## The console in a container

Not in v0.1. locksql can run its console in a container of its own
(`locksql profile add` and a Docker image); the same mode for lockmq is a
follow-up. Until then the console runs on the host, in the separated account
`lockmq install` creates.
