// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build agentdocker

package agent

import (
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/client"
	"github.com/haproxy-haptic/HAPTIC/tests/httpclient"
)

const (
	startupBudget  = 60 * time.Second
	convergeBudget = 30 * time.Second
)

// env is one pod as the chart deploys it: an HAProxy container in master-worker
// mode and an agent container, sharing the config mount, with general/ on a
// mount of its own.
type env struct {
	t              *testing.T
	image          string
	configVol      string
	generalVol     string
	haproxy        string
	agent          string
	client         *client.Client
	http           *http.Client
	reloadInterval time.Duration
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWithReloadInterval(t, time.Second)
}

func newEnvWithReloadInterval(t *testing.T, reloadInterval time.Duration) *env {
	t.Helper()
	image := requireAgentImage(t)
	name := containerName(t)
	e := &env{
		t:              t,
		image:          image,
		configVol:      name + "-cfg",
		generalVol:     name + "-general",
		haproxy:        name + "-haproxy",
		agent:          name + "-agent",
		http:           &http.Client{Timeout: 10 * time.Second},
		reloadInterval: reloadInterval,
	}
	t.Cleanup(e.teardown)
	e.createVolumes()
	e.seedBootstrap()
	e.startHAProxy()
	e.startAgent()
	return e
}

func containerName(t *testing.T) string {
	t.Helper()
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, t.Name())
	return fmt.Sprintf("haptic-agent-%s-%d", sanitized, time.Now().UnixNano()%1_000_000)
}

func (e *env) createVolumes() {
	mustDocker(e.t, "volume", "create", e.configVol)
	mustDocker(e.t, "volume", "create", e.generalVol)
}

// seedBootstrap writes the bootstrap config and the directory skeleton the
// haproxy container's start script creates in the chart.
func (e *env) seedBootstrap() {
	script := "set -e; mkdir -p " + baseDir + "/maps " + baseDir + "/ssl " + baseDir + "/general; cat > " + baseDir + "/" + configPath
	mustDockerInput(e.t, bootstrapConfig, append(e.runArgs("--rm", "-i"),
		"--entrypoint", "sh", e.image, "-c", script)...)
}

// runArgs prefixes a `docker run` with the two mounts every container of this
// pod shares.
func (e *env) runArgs(extra ...string) []string {
	args := []string{"run"}
	args = append(args, extra...)
	return append(args,
		"-v", e.configVol+":"+baseDir,
		"-v", e.generalVol+":"+baseDir+"/general")
}

func (e *env) startHAProxy() {
	mustDocker(e.t, append(e.runArgs("-d", "--name", e.haproxy,
		"-p", publishAddress()+"::"+strconv.Itoa(statsPort),
		"-p", publishAddress()+"::"+strconv.Itoa(httpPort),
		"-p", publishAddress()+"::"+strconv.Itoa(httpsPort)),
		"--entrypoint", "haproxy", e.image,
		"-dr", "-W", "-db", "-S", masterSocketPath+",level,admin", "--", baseDir+"/"+configPath)...)
	e.waitForWorker()
}

// waitForWorker blocks until the worker socket answers `show info`, which is
// the readiness gate the agent itself uses.
func (e *env) waitForWorker() {
	waitFor(e.t, "the HAProxy worker socket", startupBudget, func() error {
		out, err := runDocker(strings.NewReader("show info\n"),
			"exec", "-i", e.haproxy, "socat", "-t2", "stdio", "unix-connect:"+workerSocketPath)
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(out))
		}
		if !strings.Contains(out, "Name: HAProxy") {
			return fmt.Errorf("show info answered %q", strings.TrimSpace(out))
		}
		return nil
	})
}

func (e *env) startAgent() {
	mustDocker(e.t, append(e.runArgs("-d", "--name", e.agent,
		"-p", publishAddress()+"::"+strconv.Itoa(agentPort),
		"-p", publishAddress()+"::"+strconv.Itoa(metricsPort),
		"-e", "DATAPLANE_USERNAME="+agentUsername,
		"-e", "DATAPLANE_PASSWORD="+agentPassword),
		"--entrypoint", "/usr/local/bin/haptic", e.image,
		"agent",
		"--base-dir", baseDir,
		"--config", configPath,
		"--master-socket", "haproxy-master.sock",
		"--worker-socket", "haproxy-worker.sock",
		"--listen", ":"+strconv.Itoa(agentPort),
		"--reload-interval-min", e.reloadInterval.String(),
		"--state-file", ".haptic-agent.json",
		"--metrics-listen", ":"+strconv.Itoa(metricsPort))...)

	agentClient, err := client.New(&client.Config{
		BaseURL:            e.agentURL(),
		Username:           agentUsername,
		Password:           agentPassword,
		Timeout:            15 * time.Second,
		PerPodApplyTimeout: 2 * time.Minute,
	})
	if err != nil {
		e.t.Fatalf("agent client: %v", err)
	}
	e.client = agentClient
	e.t.Cleanup(e.client.Close)
	e.waitForReadyz()
}

func (e *env) agentURL() string {
	return fmt.Sprintf("http://%s:%d", connectHost(), publishedPort(e.t, e.agent, agentPort))
}

func (e *env) waitForReadyz() {
	waitFor(e.t, "the agent's /readyz", startupBudget, func() error {
		status, body, err := e.get(e.agentURL() + "/readyz")
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("/readyz answered %d: %s", status, body)
		}
		return nil
	})
}

func (e *env) teardown() {
	if e.t.Failed() {
		e.t.Logf("agent logs:\n%s", e.logs(e.agent))
		e.t.Logf("haproxy logs:\n%s", e.logs(e.haproxy))
	}
	for _, container := range []string{e.agent, e.haproxy} {
		_, _ = runDocker(nil, "rm", "-f", container)
	}
	for _, volume := range []string{e.configVol, e.generalVol} {
		_, _ = runDocker(nil, "volume", "rm", "-f", volume)
	}
}

func (e *env) logs(container string) string {
	out, _ := runDocker(nil, "logs", "--tail", "200", container)
	return out
}

// restartHAProxy replaces the container the way a kubelet restart does: the
// same mounts, the bootstrap config back on disk, a foreign worker for the
// agent to notice.
func (e *env) restartHAProxy() {
	mustDocker(e.t, "rm", "-f", e.haproxy)
	e.seedBootstrap()
	e.startHAProxy()
}

// worker runs one command on the worker stats socket, the socket the agent uses
// for every runtime command.
func (e *env) worker(command string) string {
	e.t.Helper()
	return e.socket(workerSocketPath, command)
}

func (e *env) socket(socket, command string) string {
	e.t.Helper()
	out, err := runDocker(strings.NewReader(command+"\n"),
		"exec", "-i", e.haproxy, "socat", "-t5", "stdio", "unix-connect:"+socket)
	if err != nil {
		e.t.Fatalf("%q on %s: %v\n%s", command, socket, err, out)
	}
	return out
}

// workerPID is the identity a reload changes; a runtime apply must leave it alone.
func (e *env) workerPID() int {
	e.t.Helper()
	for _, line := range strings.Split(e.worker("show info"), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "Pid: "); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				e.t.Fatalf("show info reported Pid %q: %v", value, err)
			}
			return pid
		}
	}
	e.t.Fatalf("show info carried no Pid:\n%s", e.worker("show info"))
	return 0
}

// read returns a file from the pod's tree, path relative to the base dir.
func (e *env) read(path string) string {
	e.t.Helper()
	return mustDocker(e.t, "exec", e.haproxy, "cat", baseDir+"/"+path)
}

// digest is the on-disk fingerprint an assertion compares before and after a
// refused apply.
func (e *env) digest(path string) string {
	e.t.Helper()
	out := mustDocker(e.t, "exec", e.haproxy, "sha256sum", baseDir+"/"+path)
	return strings.Fields(out)[0]
}

func (e *env) exists(path string) bool {
	e.t.Helper()
	_, err := runDocker(nil, "exec", e.haproxy, "test", "-e", baseDir+"/"+path)
	return err == nil
}

// device is the mount a path lives on; general/ must be its own.
// mountPoints lists the mount points below the base directory the way the
// agent's own probe reads them, from /proc/self/mountinfo. Two docker volumes
// usually share one device (so st_dev cannot tell them apart) and `stat %m`
// misreports a volume nested under a symlinked directory; a hardlink still
// cannot cross the mount, which is what the per-mount journal is about.
func (e *env) mountPoints() []string {
	e.t.Helper()
	resolved := strings.TrimSpace(mustDocker(e.t, "exec", e.haproxy, "readlink", "-f", baseDir))
	out := mustDocker(e.t, "exec", e.haproxy, "cat", "/proc/self/mountinfo")
	var points []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && strings.HasPrefix(fields[4], resolved+"/") {
			points = append(points, fields[4])
		}
	}
	return points
}

func (e *env) listAll(dir string) string {
	e.t.Helper()
	out, _ := runDocker(nil, "exec", e.haproxy, "ls", "-a", baseDir+"/"+dir)
	return out
}

func (e *env) get(url string) (status int, body string, err error) {
	resp, err := e.http.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(raw), err
}

// statsURL and httpURL address the published ports of the HAProxy container.
func (e *env) statsURL(path string) string {
	return fmt.Sprintf("http://%s:%d%s", connectHost(), publishedPort(e.t, e.haproxy, statsPort), path)
}

func (e *env) httpURL(path string) string {
	return fmt.Sprintf("http://%s:%d%s", connectHost(), publishedPort(e.t, e.haproxy, httpPort), path)
}

// requestWithHost sends one request through the HTTP frontend with a chosen
// Host header, which is what the routing map keys on.
func (e *env) requestWithHost(host, path string) (status int, header http.Header, body string) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.httpURL(path), http.NoBody)
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	req.Host = host
	resp, err := e.http.Do(req)
	if err != nil {
		e.t.Fatalf("GET %s with Host %s: %v", path, host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, string(raw)
}

// peerCertificate completes a TLS handshake for one SNI and returns the leaf
// HAProxy served, which is how a rotation is observed without a reload.
func (e *env) peerCertificate(sni string) *x509.Certificate {
	e.t.Helper()
	probe := httpclient.New(&httpclient.Config{Host: connectHost(), HTTPSPort: publishedPort(e.t, e.haproxy, httpsPort)})
	var leaf *x509.Certificate
	waitFor(e.t, "a TLS handshake for "+sni, convergeBudget, func() error {
		var err error
		leaf, err = probe.PeerCertificate(e.t.Context(), sni)
		return err
	})
	return leaf
}

// waitForReady polls the status frontend until /ready answers the expected
// status: a reload is complete only once the new worker serves it.
func (e *env) waitForReady(want int) {
	e.t.Helper()
	waitFor(e.t, fmt.Sprintf("/ready to answer %d", want), convergeBudget, func() error {
		status, body, err := e.get(e.statsURL("/ready"))
		if err != nil {
			return err
		}
		if status != want {
			return fmt.Errorf("/ready answered %d (%s)", status, strings.TrimSpace(body))
		}
		return nil
	})
}

// statRow reads one `show stat` row by column name, so a column added in a
// later HAProxy release cannot silently shift an assertion.
func (e *env) statRow(backend, server string) (map[string]string, bool) {
	e.t.Helper()
	var header []string
	for _, line := range strings.Split(e.worker("show stat"), "\n") {
		if names, ok := strings.CutPrefix(line, "# "); ok {
			header = strings.Split(strings.TrimSpace(names), ",")
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 2 || fields[0] != backend || fields[1] != server {
			continue
		}
		row := make(map[string]string, len(header))
		for i, value := range fields {
			if i < len(header) {
				row[header[i]] = value
			}
		}
		return row, true
	}
	return nil, false
}

// mustServerRow reads server's row in be-1, the backend the runtime server ops change.
func (e *env) mustServerRow(server string) map[string]string {
	e.t.Helper()
	row, ok := e.statRow("be-1", server)
	if !ok {
		e.t.Fatalf("show stat has no row for be-1/%s:\n%s", server, e.worker("show stat"))
	}
	return row
}

// requireDynamicBackends skips on a bracket older than HAProxy 3.4, the release
// that added `add backend`.
func requireDynamicBackends(t *testing.T) {
	t.Helper()
	parts := strings.SplitN(haproxyVersion(), ".", 3)
	major, _ := strconv.Atoi(parts[0])
	minor := 0
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	if major < 3 || major == 3 && minor < 4 {
		t.Skipf("`add backend` arrived in HAProxy 3.4; this bracket runs %s", haproxyVersion())
	}
}

// mapEntries parses `show map <path>` into key → value, dropping the entry id
// HAProxy prefixes each line with. Values keep every byte, including trailing
// spaces, because that is exactly what the payload form is there to preserve.
func mapEntries(output string) map[string]string {
	entries := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, "0x") {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		switch len(fields) {
		case 2:
			entries[fields[1]] = ""
		case 3:
			entries[fields[1]] = fields[2]
		}
	}
	return entries
}
