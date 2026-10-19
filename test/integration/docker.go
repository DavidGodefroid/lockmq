//go:build integration

// Package integration runs lockmq against real RabbitMQ brokers in Docker:
// one throwaway container per image, listeners reachable on 127.0.0.1
// only, removed when the test binary exits. Never point it at a real
// broker.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Credentials of the containers. The admin seeds the fixtures; the
// read-only user is the one lockmq uses.
const (
	adminUser = "itadmin"
	adminPass = "itadmin-pw"
	roUser    = "lockmq-ro"
	roPass    = "lockmq-ro-pw"
)

type broker struct {
	Image, Name, Version string
	Mgmt, AMQP, Stream   int
	hostNetwork          bool
}

func (b *broker) MgmtURL() string { return "http://127.0.0.1:" + strconv.Itoa(b.Mgmt) }

func freePorts(n int) ([]int, error) {
	var ls []net.Listener
	defer func() {
		for _, l := range ls {
			l.Close()
		}
	}()
	var out []int
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		ls = append(ls, l)
		out = append(out, l.Addr().(*net.TCPAddr).Port)
	}
	return out, nil
}

func docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// startBroker runs image with the management and stream plugins. It maps
// ports on 127.0.0.1; where the container runtime cannot (rootless podman
// without route_localnet) it falls back to the host network with every
// listener bound to 127.0.0.1.
func startBroker(image string) (*broker, error) {
	ports, err := freePorts(5)
	if err != nil {
		return nil, err
	}
	b := &broker{Image: image, Mgmt: ports[0], AMQP: ports[1], Stream: ports[2],
		Name: fmt.Sprintf("lockmq-it-%s-%d", strings.NewReplacer(":", "-", "/", "-", ".", "-").Replace(image), os.Getpid())}
	dir, err := os.MkdirTemp("", "lockmq-it-")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "enabled_plugins"), []byte("[rabbitmq_management,rabbitmq_stream].\n"), 0o644); err != nil {
		return nil, err
	}
	write := func(host bool) error {
		ip := ""
		if host {
			ip = "127.0.0.1:"
		}
		conf := fmt.Sprintf("listeners.tcp.1 = %s%d\nmanagement.tcp.port = %d\nstream.listeners.tcp.1 = %s%d\nstream.advertised_host = localhost\nstream.advertised_port = %d\nloopback_users = none\n",
			ip, b.AMQP, b.Mgmt, ip, b.Stream, b.Stream)
		if host {
			conf += "management.tcp.ip = 127.0.0.1\n"
		}
		return os.WriteFile(filepath.Join(dir, "90-it.conf"), []byte(conf), 0o644)
	}
	common := []string{"run", "-d", "--name", b.Name,
		"-e", "RABBITMQ_DEFAULT_USER=" + adminUser, "-e", "RABBITMQ_DEFAULT_PASS=" + adminPass,
		"-v", filepath.Join(dir, "enabled_plugins") + ":/etc/rabbitmq/enabled_plugins:ro",
		"-v", filepath.Join(dir, "90-it.conf") + ":/etc/rabbitmq/conf.d/90-it.conf:ro"}

	if err := write(false); err != nil {
		return nil, err
	}
	args := append([]string{}, common...)
	for _, p := range []int{b.Mgmt, b.AMQP, b.Stream} {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", p, p))
	}
	out, err := docker(append(args, image)...)
	if err != nil {
		docker("rm", "-f", b.Name)
		if err := write(true); err != nil {
			return nil, err
		}
		b.hostNetwork = true
		args = append(append([]string{}, common...), "--network", "host",
			"-e", "ERL_EPMD_ADDRESS=127.0.0.1", "-e", "ERL_EPMD_PORT="+strconv.Itoa(ports[3]),
			"-e", "RABBITMQ_DIST_PORT="+strconv.Itoa(ports[4]), "-e", "RABBITMQ_NODENAME=rabbit@localhost", image)
		if out2, err := docker(args...); err != nil {
			docker("rm", "-f", b.Name)
			return nil, fmt.Errorf("docker run %s: %v\n%s\n%s", image, err, out, out2)
		}
	}
	if err := b.waitReady(3 * time.Minute); err != nil {
		logs, _ := docker("logs", "--tail", "40", b.Name)
		b.Remove()
		return nil, fmt.Errorf("%s: %v\n%s", image, err, logs)
	}
	return b, nil
}

func (b *broker) Remove() { docker("rm", "-f", b.Name) }

func (b *broker) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var ov struct {
			Version string `json:"rabbitmq_version"`
		}
		if code, _ := b.admin("GET", "/api/overview", nil, &ov); code == 200 && ov.Version != "" {
			if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(b.Stream), time.Second); err == nil {
				c.Close()
				b.Version = ov.Version
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("not ready after %s", timeout)
}

// call sends one management API request as user.
func (b *broker) call(user, pass, method, path string, body, into any) (int, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, b.MgmtURL()+path, rd)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if into != nil && len(data) > 0 && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, into); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func (b *broker) admin(method, path string, body, into any) (int, error) {
	return b.call(adminUser, adminPass, method, path, body, into)
}

func (b *broker) mustAdmin(method, path string, body any) error {
	code, err := b.admin(method, path, body, nil)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("%s %s: HTTP %d", method, path, code)
	}
	return nil
}

func q(s string) string { return url.PathEscape(s) }

// publish publishes one message through the default exchange.
func (b *broker) publish(vhost, routingKey, payload string, props map[string]any) error {
	if props == nil {
		props = map[string]any{}
	}
	var res struct {
		Routed bool `json:"routed"`
	}
	code, err := b.admin("POST", "/api/exchanges/"+q(vhost)+"/amq.default/publish", map[string]any{
		"properties": props, "routing_key": routingKey, "payload": payload, "payload_encoding": "string",
	}, &res)
	if err != nil {
		return err
	}
	if code >= 300 || !res.Routed {
		return fmt.Errorf("publish to %s: HTTP %d routed %v", routingKey, code, res.Routed)
	}
	return nil
}

// queueDepth reads a queue's message count through rabbitmqctl, which
// does not lag like the management statistics.
func (b *broker) queueDepth(name string) (int, error) {
	out, err := docker("exec", b.Name, "rabbitmqctl", "-q", "list_queues", "name", "messages")
	if err != nil {
		return 0, fmt.Errorf("%v: %s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == name {
			return strconv.Atoi(f[1])
		}
	}
	return 0, fmt.Errorf("queue %s not listed", name)
}
