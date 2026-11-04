// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"time"
)

const snapshot = `date -u +%s.%N
for path in /proc/stat /proc/net/netstat /proc/net/snmp /proc/pressure/cpu /proc/pressure/io /proc/pressure/memory; do
  echo "$path"
  cat "$path"
done
find /sys/fs/cgroup -maxdepth 6 -name cpu.stat | while IFS= read -r path; do
  echo "$path"
  cat "$path"
  dir="${path%/*}"
  cat "$dir/cpu.weight" "$dir/cpu.pressure" 2>/dev/null || true
done
`

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 34*time.Minute)
	defer cancel()
	file, err := os.Create(".pilot/scheduling.jsonl")
	if err != nil {
		panic(err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(".pilot/test-finished"); err == nil {
				return
			}
			capture(ctx, encoder)
		}
	}
}

func capture(ctx context.Context, encoder *json.Encoder) {
	commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	started := time.Now()
	out, err := exec.CommandContext(commandCtx, "docker", "exec", "haptic-e2e-control-plane", "sh", "-c", snapshot).CombinedOutput()
	failure := ""
	if err != nil {
		failure = err.Error()
	}
	if err := encoder.Encode(map[string]any{
		"started": started.UTC(), "elapsed_ms": time.Since(started).Milliseconds(),
		"output": string(out), "error": failure,
	}); err != nil {
		panic(err)
	}
}
