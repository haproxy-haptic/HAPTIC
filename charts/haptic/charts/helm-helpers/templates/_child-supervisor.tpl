{{- define "haptic.childSupervisor.shellFunctions" -}}
_child_pid=
_watchdog_pid=
_watch_interval_seconds=3
_watch_timeout_seconds=2
_watch_failure_threshold=3
_watch_success_threshold=2
_watch_term_grace_seconds=3
_watch_startup_grace_seconds=120

_shutdown() {
  if [ -n "$_watchdog_pid" ]; then
    kill -TERM "$_watchdog_pid" 2>/dev/null || true
    wait "$_watchdog_pid" 2>/dev/null || true
    _watchdog_pid=
  fi
  if [ -n "$_child_pid" ]; then
    kill -TERM "$_child_pid" 2>/dev/null || true
    wait "$_child_pid" 2>/dev/null || true
  fi
  exit 0
}
trap _shutdown TERM INT

_watch_child() {
  _watched_pid=$1
  trap 'exit 0' TERM INT
  if ! command -v perl >/dev/null 2>&1 || ! command -v timeout >/dev/null 2>&1; then
    echo "WARNING: ${_child_name} watchdog is disabled because perl or timeout is unavailable in the sidecar image"
    return
  fi
  _health_failures=0
  _health_successes=0
  _health_seen=0
  _watch_started_epoch_seconds=$(date +%s)
  sleep "$_watch_interval_seconds"
  while kill -0 "$_watched_pid" 2>/dev/null; do
    _child_health
    _health_status=$?
    case "$_health_status" in
      0)
        _health_successes=$((_health_successes + 1))
        if [ "$_health_successes" -ge "$_watch_success_threshold" ]; then
          _health_seen=1
        fi
        _health_failures=0
        ;;
      1)
        _health_successes=0
        _watch_runtime_seconds=$(( $(date +%s) - _watch_started_epoch_seconds ))
        if [ "$_health_seen" -eq 1 ] || [ "$_watch_runtime_seconds" -ge "$_watch_startup_grace_seconds" ]; then
          _health_failures=$((_health_failures + 1))
        else
          _health_failures=0
        fi
        ;;
      *)
        _health_successes=0
        _health_failures=0
        ;;
    esac
    if [ "$_health_failures" -ge "$_watch_failure_threshold" ]; then
      echo "WARNING: ${_child_name} failed ${_health_failures} consecutive health checks; ${_child_impact}; restarting child"
      kill -TERM "$_watched_pid" 2>/dev/null || true
      _term_waited=0
      while kill -0 "$_watched_pid" 2>/dev/null && [ "$_term_waited" -lt "$_watch_term_grace_seconds" ]; do
        sleep 1
        _term_waited=$((_term_waited + 1))
      done
      if kill -0 "$_watched_pid" 2>/dev/null; then
        kill -KILL "$_watched_pid" 2>/dev/null || true
      fi
      return
    fi
    sleep "$_watch_interval_seconds"
  done
}

_start_watchdog() {
  _watch_child "$_child_pid" &
  _watchdog_pid=$!
}

_stop_watchdog() {
  if [ -n "$_watchdog_pid" ]; then
    kill -TERM "$_watchdog_pid" 2>/dev/null || true
    wait "$_watchdog_pid" 2>/dev/null || true
    _watchdog_pid=
  fi
}
{{- end -}}
