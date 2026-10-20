// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/haproxy-haptic/HAPTIC/tests/process"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

const diagnosticStreamLimit = 8 << 20

type diagnosticOutput struct {
	mu         sync.Mutex
	file       *os.File
	compressed *gzip.Writer
	remaining  int
	err        error
}

func newDiagnosticOutput(path string, limit int) (*diagnosticOutput, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(filepath.Base(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &diagnosticOutput{file: file, compressed: gzip.NewWriter(file), remaining: limit}, nil
}

func (o *diagnosticOutput) Write(payload []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return 0, o.err
	}
	record, err := json.Marshal(struct {
		Observed string `json:"observed"`
		Output   string `json:"output"`
	}{time.Now().UTC().Format(time.RFC3339Nano), string(payload)})
	if err != nil {
		return 0, err
	}
	record = append(record, '\n')
	if len(record) > o.remaining {
		o.err = fmt.Errorf("diagnostic capture %s reached its byte limit", o.file.Name())
		return 0, o.err
	}
	o.remaining -= len(record)
	_, o.err = o.compressed.Write(record)
	if o.err == nil {
		o.err = o.compressed.Flush()
	}
	return len(payload), o.err
}

func (o *diagnosticOutput) close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return errors.Join(o.err, o.compressed.Close(), o.file.Close())
}

type diagnosticProcess struct {
	name   string
	output *diagnosticOutput
	child  process.Running
}

type upgradeDiagnostics struct {
	session    *Session
	directory  string
	cancel     context.CancelFunc
	processes  []diagnosticProcess
	containers []string
	samples    *testutil.ObservationProbe
	nodeOutput *diagnosticOutput
	nodeError  error
}

func (s *Session) observeUpgradeDiagnostics(ctx context.Context) (func() error, error) {
	directory := filepath.Join(s.Artifacts, "upgrade-diagnostics")
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, err
	}
	observeCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	d := &upgradeDiagnostics{session: s, directory: directory, cancel: cancel}
	if err := d.start(observeCtx); err != nil {
		return nil, errors.Join(err, d.finish())
	}
	return d.finish, nil
}

func (d *upgradeDiagnostics) start(ctx context.Context) error {
	for _, resource := range []string{podsResource, "services", "endpointslices"} {
		args := []string{kubectlGet, resource, "--watch", "--output-watch-events", "-o", "json"}
		if resource == podsResource {
			args = append(args, "--all-namespaces")
		}
		command, err := d.session.Client.Command(nil, args...)
		if err != nil {
			return err
		}
		if err := d.startStream(ctx, resource, command); err != nil {
			return err
		}
	}
	nodes, err := d.session.Cluster.OwnedNodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) != 1 {
		return fmt.Errorf("upgrade diagnostics needs one owned Kind node, got %d", len(nodes))
	}
	if err := d.startConntrack(ctx, nodes[0]); err != nil {
		return err
	}
	d.nodeOutput, err = newDiagnosticOutput(filepath.Join(d.directory, "node.jsonl.gz"), 64<<20)
	if err != nil {
		return err
	}
	d.samples, err = testutil.StartObservationProbe(ctx, time.Second, func(ctx context.Context) error {
		return d.sampleNode(ctx, nodes[0])
	})
	return err
}

func (d *upgradeDiagnostics) startStream(ctx context.Context, name string, command *process.Command) error {
	output, err := newDiagnosticOutput(filepath.Join(d.directory, name+".jsonl.gz"), diagnosticStreamLimit)
	if err != nil {
		return err
	}
	command.Stdout, command.Stderr = output, output
	command.Env = d.session.Cluster.Environment
	child, err := d.session.Runner.Start(ctx, command)
	if err != nil {
		return errors.Join(err, output.close())
	}
	d.processes = append(d.processes, diagnosticProcess{name, output, child})
	return nil
}

func (d *upgradeDiagnostics) startConntrack(ctx context.Context, node string) error {
	s := d.session
	deployment, err := s.Deployment(ctx, "-haproxy")
	if err != nil {
		return err
	}
	service, err := readJSON[corev1.Service](ctx, s, kubectlGet, "service", deployment.Name)
	if err != nil {
		return err
	}
	if service.Spec.ClusterIP == "" || service.Spec.ClusterIP == corev1.ClusterIPNone {
		return fmt.Errorf("service %s has no ClusterIP for connection diagnostics", deployment.Name)
	}
	image, err := s.Run(ctx, "docker", "inspect", "--format", "{{.Image}}", node)
	if err != nil {
		return err
	}
	created, err := s.Run(ctx, "docker", "create", "--network", "container:"+node,
		"--cap-add", "NET_ADMIN", "--entrypoint", "conntrack", strings.TrimSpace(image.Stdout),
		"-E", "-p", "tcp", "--orig-dst", service.Spec.ClusterIP, "-o", "timestamp,extended")
	if err != nil {
		return err
	}
	id := strings.TrimSpace(created.Stdout)
	d.containers = append(d.containers, id)
	return d.startStream(ctx, "conntrack", &process.Command{Name: dockerCommand, Args: []string{"start", "--attach", id}})
}

func (d *upgradeDiagnostics) sampleNode(ctx context.Context, node string) error {
	started := time.Now().UTC()
	sampleCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := d.session.Runner.Run(sampleCtx, &process.Command{
		Name: dockerCommand, Args: []string{"exec", node, "bash", "-o", "pipefail", "-c",
			`timeout 2 bash -o pipefail -c "$1" bash "$2" 2>&1 | head -c 262145`, "bash", upgradeNodeSample, d.session.Namespace},
		Env: d.session.Cluster.Environment,
	})
	if len(result.Stdout) > 256<<10 {
		err = errors.Join(err, errors.New("upgrade node sample reached its byte limit"))
	}
	payload := fmt.Sprintf("started=%s duration=%s exit=%d error=%v\n%s\n%s",
		started.Format(time.RFC3339Nano), time.Since(started), result.ExitCode, err, result.Stdout, result.Stderr)
	if err != nil && ctx.Err() == nil && d.nodeError == nil {
		d.nodeError = fmt.Errorf("sample upgrade node: %w", err)
	}
	_, err = d.nodeOutput.Write([]byte(payload))
	return err
}

func (d *upgradeDiagnostics) finish() error {
	var result error
	if d.samples != nil {
		result = errors.Join(result, d.samples.Stop(), d.nodeError)
	}
	if d.nodeOutput != nil {
		result = errors.Join(result, d.nodeOutput.close())
	}
	for _, stream := range d.processes {
		select {
		case <-stream.child.Done():
			record, err := stream.child.Wait()
			result = errors.Join(result, fmt.Errorf("diagnostic stream %s ended before upgrade completed (exit %d): %v", stream.name, record.ExitCode, err))
		default:
		}
	}
	d.cancel()
	for _, stream := range d.processes {
		result = errors.Join(result, stream.child.Stop(), stream.output.close())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, id := range d.containers {
		_, err := d.session.Run(ctx, "docker", "rm", "--force", id)
		result = errors.Join(result, err)
	}
	report := fmt.Sprintf("capture_error=%v\n", result)
	result = errors.Join(result, os.WriteFile(filepath.Join(d.directory, "result.txt"), []byte(report), 0o600))
	return result
}

const upgradeNodeSample = `
status=0
record_status() {
  result=$2
  printf '%s_exit=%s\n' "$1" "$result"
  if [ "$result" -ne 0 ]; then status=1; fi
}
printf '# HAProxy and agent CRI tails\n'
for directory in /var/log/pods/"$1"_*/haproxy /var/log/pods/"$1"_*/agent; do
  [ -d "$directory" ] || continue
  for file in "$directory"/*.log; do
    [ -f "$file" ] || continue
    printf '%s\n' "$file"
    tail -c 8192 "$file"
    result=$?
    if [ "$result" -ne 0 ] && [ ! -f "$file" ]; then
      printf 'retired_log_disappeared=%s\n' "$file"
    else
      record_status "$file" "$result"
    fi
  done
done
printf '# iptables\n'
iptables-save -t nat
record_status iptables "$?"
printf '# proxy metrics\n'
curl --silent --show-error --max-time 1 http://127.0.0.1:10249/metrics | grep -E '^(kubeproxy_|process_cpu_seconds|go_goroutines)'
record_status proxy_metrics "$?"
printf '# cgroups\n'
find /sys/fs/cgroup -ignore_readdir_race -maxdepth 7 -type f \( -name cpu.stat -o -name cpu.pressure -o -name cpu.weight -o -name cpu.max -o -name cpuset.cpus.effective \) -exec bash -c '
status=0
for file do
  printf "%s\n" "$file"
  if content=$(<"$file"); then
    printf "%s\n" "$content"
  elif [ -f "$file" ]; then
    status=1
  else
    printf "retired_cgroup_disappeared=%s\n" "$file"
  fi
done
exit "$status"' sh {} +
record_status cgroups "$?"
exit "$status"
`
