#!/usr/bin/env bash

set -euo pipefail

# Ensure we operate from the repo root regardless of where the script is invoked
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ASSETS_DIR="${SCRIPT_DIR}/dev-env-assets"

# Source centralized version configuration
# shellcheck source=../versions.env
source "${REPO_ROOT}/versions.env"
# shellcheck source=lib/cluster.sh
source "${SCRIPT_DIR}/lib/cluster.sh"

# Default configuration
# Naming conventions - defaults match chart name for clean resource names
# Override via environment variables if needed
CLUSTER_NAME="${CLUSTER_NAME:-haptic-dev}"
CTRL_NAMESPACE="${CTRL_NAMESPACE:-haptic}"
HELM_RELEASE_NAME="${HELM_RELEASE_NAME:-haptic}"

ECHO_NAMESPACE="echo"
ECHO_APP_NAME="echo-server"
ECHO_IMAGE="ealen/echo-server@sha256:ec8a6e95890df937a1eb5fafca033a32172d4f43c1fea1f302931d5f230a137f"
LOCAL_IMAGE="haptic:dev"
HAPROXY_VERSION="${HAPROXY_VERSION:-$DEFAULT_HAPROXY}"
HAPROXY_ENTERPRISE="${HAPROXY_ENTERPRISE:-false}"
TIMEOUT="180"
SKIP_BUILD=false
SKIP_ECHO=false
FORCE_REBUILD=false
VERBOSE=false
METRICS_RAW=false
METRICS_WATCH=false

RED="\033[0;31m"
GREEN="\033[0;32m"
YELLOW="\033[1;33m"
BLUE="\033[0;34m"
NC="\033[0m"

log() {
	local level msg
	level="$1"; shift
	msg="$*"
	echo -e "${BLUE}[$(date +'%Y-%m-%dT%H:%M:%S%z')]${NC} ${level}: ${msg}"
}

ok() { echo -e "${GREEN}✔${NC} $*"; }
warn() { echo -e "${YELLOW}⚠${NC} $*"; }
err() { echo -e "${RED}✖${NC} $*"; }
debug() { [[ "$VERBOSE" == "true" ]] && echo -e "${BLUE}[DEBUG]${NC} $*" || true; }

# Docker-in-Docker detection and hostname extraction
# When DOCKER_HOST is set (e.g., tcp://docker:2376), the Docker daemon runs in a
# separate container and kind clusters are accessible via that hostname, not localhost.
is_docker_in_docker() {
    [[ "${DOCKER_HOST:-}" == tcp://* ]]
}

# Extract hostname from DOCKER_HOST (e.g., tcp://docker:2376 → docker)
# Returns "localhost" if not in dind environment
get_dind_hostname() {
    if is_docker_in_docker; then
        echo "$DOCKER_HOST" | sed 's|tcp://||' | cut -d: -f1
    else
        echo "localhost"
    fi
}

# Get the host to use for NodePort access (localhost or dind hostname)
NODEPORT_HOST="$(get_dind_hostname)"

# Compute HAProxy image settings based on enterprise mode
compute_haproxy_image() {
    if [[ "$HAPROXY_ENTERPRISE" == "true" ]]; then
        HAPROXY_REPO="hapee-registry.haproxy.com/haproxy-enterprise"
        # Map community version to the tested Enterprise revision tag.
        case "$HAPROXY_VERSION" in
            3.0*) HAPROXY_TAG="$HAPROXY_ENTERPRISE_30" ;;
            3.2*) HAPROXY_TAG="$HAPROXY_ENTERPRISE_32" ;;
            *)    HAPROXY_TAG="$HAPROXY_VERSION" ;;
        esac
        debug "Using HAProxy Enterprise: ${HAPROXY_REPO}:${HAPROXY_TAG}"
    else
        HAPROXY_REPO="haproxytech/haproxy-debian"
        HAPROXY_TAG="$HAPROXY_VERSION"
        debug "Using HAProxy Community: ${HAPROXY_REPO}:${HAPROXY_TAG}"
    fi
}

print_section() {
    echo
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo " $1"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
}

show_spinner() {
    local pid=$1
    local message="$2"
    local spin='⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏'
    local i=0

    while kill -0 "$pid" 2>/dev/null; do
        i=$(( (i+1) %10 ))
        printf "\r%s%s%s %s" "$BLUE" "${spin:$i:1}" "$NC" "$message"
        sleep 0.1
    done

    printf "\r"  # Clear the spinner line
}

run_with_spinner() {
    local message="$1"
    shift
    local command=("$@")

    if [[ "$VERBOSE" == "true" ]]; then
        log INFO "$message"
        "${command[@]}"
    else
        "${command[@]}" &
        local pid=$!
        show_spinner $pid "$message"
        wait $pid
        local exit_code=$?

        if [[ $exit_code -eq 0 ]]; then
            ok "$message"
        else
            err "$message (exit code: $exit_code)"
        fi

        return $exit_code
    fi
}

show_usage() {
    cat <<EOF
Usage: $0 [COMMAND] [OPTIONS]

COMMANDS:
    up          Start development environment (default)
    down        Delete the kind cluster
    logs        Follow controller logs
    exec        Execute shell in controller pod
    restart     Rebuild image and restart controller (use --skip-build to skip)
    status      Show deployment status
    metrics     Show controller Prometheus metrics
    clean       Clean up and reset environment
    test        Test ingress controller functionality
    port-forward Setup port forwarding for HAProxy services

OPTIONS:
    --cluster-name NAME     Custom cluster name (default: haptic-dev)
    --namespace NAME        Custom controller namespace (default: haptic)
    --image-tag TAG         Custom image tag (default: dev)
    --timeout SECONDS       Deployment timeout (default: 180)
    --skip-build            Use existing image if available
    --skip-echo             Don't deploy echo server
    --force-rebuild         Force rebuild without cache
    --verbose               Enable debug output
    --help, -h              Show this help

ENVIRONMENT VARIABLES:
    HAPROXY_VERSION         HAProxy version for production pods (default: ${DEFAULT_HAPROXY})
                            Supported: ${HAPROXY_VERSIONS}
    HAPROXY_ENTERPRISE      Use HAProxy Enterprise instead of Community (default: false)
                            Enterprise versions: 3.0r1, 3.2r1
                            Registry: hapee-registry.haproxy.com/haproxy-enterprise

EXAMPLES:
    $0                      # Start dev environment with defaults
    $0 up --skip-build      # Start without rebuilding image
    $0 restart              # Rebuild image and restart controller
    $0 restart --skip-build # Restart controller without rebuilding
    $0 metrics              # Show controller metrics
    $0 metrics --watch      # Continuously monitor metrics
    $0 metrics --raw        # Show raw Prometheus format
    $0 test                 # Test ingress controller functionality
    $0 logs                 # Follow controller logs
    $0 port-forward         # Setup port forwarding for testing
    $0 down                 # Delete cluster
    $0 restart --verbose    # Restart with debug output
    HAPROXY_VERSION=3.0 $0  # Use HAProxy 3.0 instead of default 3.2
    HAPROXY_ENTERPRISE=true $0          # Use HAProxy Enterprise 3.2r1
    HAPROXY_ENTERPRISE=true HAPROXY_VERSION=3.0 $0  # Use HAProxy Enterprise 3.0r1

EOF
}

parse_args() {
    COMMAND=""

    # Parse command if provided
    if [[ $# -gt 0 ]] && [[ "$1" != --* ]]; then
        COMMAND="$1"
        shift
    fi

    # Default to 'up' if no command specified
    [[ -z "$COMMAND" ]] && COMMAND="up"

    # Parse options
    while [[ $# -gt 0 ]]; do
        case $1 in
            --cluster-name)
                CLUSTER_NAME="$2"
                shift 2
                ;;
            --namespace)
                CTRL_NAMESPACE="$2"
                shift 2
                ;;
            --image-tag)
                LOCAL_IMAGE="haptic:$2"
                shift 2
                ;;
            --timeout)
                TIMEOUT="$2"
                shift 2
                ;;
            --skip-build)
                SKIP_BUILD=true
                shift
                ;;
            --skip-echo)
                SKIP_ECHO=true
                shift
                ;;
            --force-rebuild)
                FORCE_REBUILD=true
                shift
                ;;
            --verbose)
                VERBOSE=true
                shift
                ;;
            --raw)
                METRICS_RAW=true
                shift
                ;;
            --watch)
                METRICS_WATCH=true
                shift
                ;;
            --help|-h)
                show_usage
                exit 0
                ;;
            *)
                err "Unknown option: $1"
                show_usage
                exit 1
                ;;
        esac
    done

    debug "Command: $COMMAND"
    debug "Cluster: $CLUSTER_NAME"
    debug "Namespace: $CTRL_NAMESPACE"
    debug "Image: $LOCAL_IMAGE"
    debug "Timeout: ${TIMEOUT}s"
}

require_cmd() {
	if ! command -v "$1" >/dev/null 2>&1; then
		err "Required command '$1' not found in PATH."
		troubleshooting "missing-$1"
		exit 1
	fi
}

cleanup_on_exit() {
    local exit_code=$?
    debug "Cleanup function called with exit code: $exit_code"

    if [[ $exit_code -ne 0 ]]; then
        warn "Script exited with error code $exit_code"
        warn "Run with --verbose for more detailed output"

        # Offer helpful next steps
        if kind get clusters 2>/dev/null | grep -q "$CLUSTER_NAME"; then
            warn "Cluster '$CLUSTER_NAME' exists. You can:"
            echo "  - Check status: $0 status"
            echo "  - Clean up: $0 down"
            echo "  - Retry: $0 up"
        fi
    fi

    # Always try to return to original directory
    popd >/dev/null 2>&1 || true
}

cleanup_failed_deployment() {
    warn "Cleaning up failed deployment..."

    # Check if namespace exists before attempting cleanup
    if ! kubectl get namespace "$CTRL_NAMESPACE" >/dev/null 2>&1; then
        debug "Namespace '$CTRL_NAMESPACE' does not exist, skipping cleanup"
        return 0
    fi

    # Delete any failed pods to allow clean retry
    debug "Removing failed pods..."
    kubectl -n "$CTRL_NAMESPACE" delete pods --field-selector=status.phase=Failed 2>/dev/null || true

    # Delete pods stuck in ImagePullBackOff or CrashLoopBackOff
    debug "Removing stuck pods..."
    kubectl -n "$CTRL_NAMESPACE" get pods -o json 2>/dev/null | \
        jq -r '.items[] | select((.status.initContainerStatuses[]?, .status.containerStatuses[]?) | .state.waiting? | .reason? | test("ImagePullBackOff|CrashLoopBackOff|ErrImagePull")) | .metadata.name' | \
        xargs -r kubectl -n "$CTRL_NAMESPACE" delete pod 2>/dev/null || true

    # Rollback Helm release if it exists and has previous revisions
    if helm list -n "$CTRL_NAMESPACE" 2>/dev/null | grep -q "$HELM_RELEASE_NAME"; then
        local revision_count
        revision_count=$(helm history "$HELM_RELEASE_NAME" -n "$CTRL_NAMESPACE" 2>/dev/null | wc -l)

        # Only rollback if there are previous revisions (more than 2 lines: header + current)
        if [[ $revision_count -gt 2 ]]; then
            debug "Rolling back Helm release..."
            helm rollback "$HELM_RELEASE_NAME" -n "$CTRL_NAMESPACE" 2>/dev/null || true
        else
            debug "No previous Helm revision to rollback to, uninstalling release..."
            helm uninstall "$HELM_RELEASE_NAME" -n "$CTRL_NAMESPACE" 2>/dev/null || true
        fi
    fi

    ok "Failed deployment cleanup completed"
}

retry_with_backoff() {
    local max_attempts="$1"
    local delay="$2"
    local command=("${@:3}")
    local attempt=1

    while [[ $attempt -le $max_attempts ]]; do
        debug "Attempt $attempt/$max_attempts: ${command[*]}"

        if "${command[@]}"; then
            debug "Command succeeded on attempt $attempt"
            return 0
        fi

        if [[ $attempt -lt $max_attempts ]]; then
            warn "Command failed (attempt $attempt/$max_attempts), retrying in ${delay}s..."
            sleep "$delay"
            delay=$((delay * 2))  # Exponential backoff
        fi

        ((attempt++)) || true
    done

    err "Command failed after $max_attempts attempts: ${command[*]}"
    return 1
}

troubleshooting() {
	case "$1" in
		missing-kind)
			warn "Install kind: https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
			;;
		missing-kubectl)
			warn "Install kubectl: https://kubernetes.io/docs/tasks/tools/"
			;;
		missing-docker)
			warn "Install Docker: https://docs.docker.com/get-docker/ and ensure your user can run it (linux: add to docker group)."
			;;
		missing-helm)
			warn "Install Helm: https://helm.sh/docs/intro/install/"
			;;
		*) ;;
	 esac
}

ensure_cluster() {
	if ! kind get clusters | grep -qx "${CLUSTER_NAME}"; then
		log INFO "Creating kind cluster '${CLUSTER_NAME}'..."
		kind create cluster --name "${CLUSTER_NAME}" --config "${ASSETS_DIR}/kind-config.yaml"
		kind_blackhole_synthetic_backends "${CLUSTER_NAME}"
		kind_install_kindnet "${CLUSTER_NAME}"
		ok "Cluster created with admission controllers enabled."
	else
		ok "Using existing cluster '${CLUSTER_NAME}'."
		# Export kubeconfig for existing cluster to ensure context is up-to-date
		debug "Exporting kubeconfig for existing cluster..."
		kind export kubeconfig --name "${CLUSTER_NAME}" >/dev/null 2>&1
	fi

	# If running in Docker-in-Docker, patch the kubeconfig to use the dind hostname
	# instead of localhost/127.0.0.1 (which are not accessible from the CI job container)
	if is_docker_in_docker; then
		local dind_hostname
		dind_hostname=$(get_dind_hostname)
		log INFO "Detected Docker-in-Docker, patching kubeconfig to use '${dind_hostname}'..."

		# Get kubeconfig path
		local kubeconfig_path="${KUBECONFIG:-$HOME/.kube/config}"

		# Replace localhost/127.0.0.1 with dind hostname in kubeconfig
		if [[ -f "$kubeconfig_path" ]]; then
			sed -i "s|https://127.0.0.1:|https://${dind_hostname}:|g" "$kubeconfig_path"
			sed -i "s|https://0.0.0.0:|https://${dind_hostname}:|g" "$kubeconfig_path"
			sed -i "s|https://localhost:|https://${dind_hostname}:|g" "$kubeconfig_path"
			ok "Kubeconfig patched for Docker-in-Docker (server: https://${dind_hostname}:...)"
		fi
	fi

	local ctx="kind-${CLUSTER_NAME}"
	if ! kubectl config get-contexts -o name | grep -qx "$ctx"; then
		err "kubectl context '$ctx' not found."
		err "Available contexts:"
		kubectl config get-contexts -o name
		exit 1
	fi
	kubectl config use-context "$ctx" >/dev/null
	ok "Context configured."

	# Install Gateway API CRDs if not already present
	# This must happen before Helm install so .Capabilities.APIVersions.Has can detect them
	if ! kubectl get crd gatewayclasses.gateway.networking.k8s.io >/dev/null 2>&1; then
		# Version comes from go.mod, never a literal here: a second copy goes stale
		# the moment sigs.k8s.io/gateway-api is bumped, and the dev cluster then
		# carries different CRDs than the e2e suite installs.
		local gwapi_version
		gwapi_version=$(go list -C "${REPO_ROOT}" -m -f '{{.Version}}' sigs.k8s.io/gateway-api)
		log INFO "Installing Gateway API CRDs (standard channel ${gwapi_version})..."
		kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${gwapi_version}/standard-install.yaml" >/dev/null 2>&1

		# Wait for CRDs to be established
		log INFO "Waiting for Gateway API CRDs to be established..."
		kubectl wait --for condition=established --timeout=60s \
			crd/gatewayclasses.gateway.networking.k8s.io \
			crd/gateways.gateway.networking.k8s.io \
			crd/httproutes.gateway.networking.k8s.io \
			crd/grpcroutes.gateway.networking.k8s.io >/dev/null 2>&1
		ok "Gateway API CRDs installed"
	else
		debug "Gateway API CRDs already installed"
	fi
}

# Verify and auto-switch kubectl context (CONVENIENCE + SAFETY)
# Automatically switches to the dev cluster context if it exists.
# Only fails if the expected context doesn't exist at all.
verify_cluster_context() {
	local expected_ctx="kind-${CLUSTER_NAME}"
	local current_ctx
	current_ctx=$(kubectl config current-context 2>/dev/null || echo "")

	# Check if we're already on the right context
	if [[ "$current_ctx" == "$expected_ctx" ]]; then
		debug "Context verified: $current_ctx ✓"
		return 0
	fi

	# Not on right context - check if expected context exists
	if ! kubectl config get-contexts -o name | grep -qx "$expected_ctx"; then
		err "Kind cluster context '$expected_ctx' not found!"
		err "Available contexts:"
		kubectl config get-contexts -o name | sed 's/^/  /'
		err ""
		err "The development cluster doesn't appear to exist."
		err "Run '$0 up' first to create and initialize it."
		exit 1
	fi

	# Context exists, switch to it automatically
	if [[ -n "$current_ctx" ]]; then
		warn "Switching kubectl context: $current_ctx → $expected_ctx"
	else
		log INFO "Setting kubectl context to: $expected_ctx"
	fi

	kubectl config use-context "$expected_ctx" >/dev/null
	ok "Context switched to development cluster"
}

setup_webhook_certificates() {
    local webhook_enabled
    local secret_name="${HELM_RELEASE_NAME}-webhook-tls"
    local certs_dir="/tmp/haproxy-webhook-certs"

    # Check if webhook is enabled in values
    webhook_enabled=$(grep -A5 "^webhook:" "${ASSETS_DIR}/dev-values.yaml" | grep "enabled:" | head -1 | awk '{print $2}' || echo "false")

    if [[ "$webhook_enabled" != "true" ]]; then
        debug "Webhook not enabled, skipping certificate setup"
        return 0
    fi

    log INFO "Setting up webhook certificates..."

    # Check if Secret already exists with valid certificates
    if kubectl -n "${CTRL_NAMESPACE}" get secret "$secret_name" >/dev/null 2>&1; then
        # Check if certificates are still valid (> 7 days remaining)
        local tls_crt
        tls_crt=$(kubectl -n "${CTRL_NAMESPACE}" get secret "$secret_name" -o jsonpath='{.data.tls\.crt}' | base64 -d)

        if echo "$tls_crt" | openssl x509 -checkend $((7 * 24 * 3600)) -noout 2>/dev/null; then
            ok "Existing webhook certificates are still valid"

            # Extract CA bundle for Helm
            WEBHOOK_CA_BUNDLE=$(kubectl -n "${CTRL_NAMESPACE}" get secret "$secret_name" -o jsonpath='{.data.ca\.crt}')
            export WEBHOOK_CA_BUNDLE
            debug "Exported WEBHOOK_CA_BUNDLE from existing Secret"
            return 0
        else
            warn "Existing certificates expire soon, regenerating..."
            kubectl -n "${CTRL_NAMESPACE}" delete secret "$secret_name" 2>/dev/null || true
        fi
    fi

    # Generate new certificates
    log INFO "Generating self-signed webhook certificates..."
    "${ASSETS_DIR}/generate-webhook-certs.sh" \
        "${CTRL_NAMESPACE}" \
        "${HELM_RELEASE_NAME}-webhook" \
        "$certs_dir" || {
        err "Failed to generate webhook certificates"
        return 1
    }

    ok "Webhook certificates generated"

    # Create Secret with certificates
    log INFO "Creating webhook certificate Secret..."
    kubectl create secret generic "$secret_name" \
        --from-file=tls.crt="$certs_dir/tls.crt" \
        --from-file=tls.key="$certs_dir/tls.key" \
        --from-file=ca.crt="$certs_dir/ca.crt" \
        --namespace="${CTRL_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f - || {
        err "Failed to create webhook certificate Secret"
        return 1
    }

    ok "Webhook certificate Secret created"

    # Export CA bundle for Helm (base64-encoded CA cert)
    WEBHOOK_CA_BUNDLE=$(cat "$certs_dir/ca-bundle.b64")
    export WEBHOOK_CA_BUNDLE
    debug "Exported WEBHOOK_CA_BUNDLE=${WEBHOOK_CA_BUNDLE:0:20}..."

    # Clean up temporary certificates
    rm -rf "$certs_dir"

    return 0
}

build_and_load_local_image() {
    # Calculate source hash for reproducible builds and verification
    local source_hash
    source_hash=$("${SCRIPT_DIR}/source-hash.sh")
    debug "Source hash: ${source_hash}"

    # Build image unless --skip-build is set
    if [[ "$SKIP_BUILD" != "true" ]]; then
        local build_args=("-t" "${LOCAL_IMAGE}")

        local haproxy_image
        haproxy_image="$(bash "${SCRIPT_DIR}/chart-haproxy-image.sh" "${HAPROXY_VERSION}")" || return 1
        build_args+=("--build-arg" "HAPROXY_IMAGE=${haproxy_image}")

        # Pass source hash to embed in binary
        build_args+=("--build-arg" "SOURCE_HASH=${source_hash}")

        if [[ "$FORCE_REBUILD" == "true" ]]; then
            build_args+=("--no-cache")
        fi

        build_args+=("${REPO_ROOT}")

        local docker_path
        docker_path="$(command -v docker)"

        run_with_spinner "Building controller image '${LOCAL_IMAGE}' with HAProxy ${HAPROXY_VERSION}" \
            env DOCKER_BUILDKIT=1 "${docker_path}" build "${build_args[@]}" || {
            err "Docker build failed"
            return 1
        }
    elif docker image inspect "${LOCAL_IMAGE}" >/dev/null 2>&1; then
        ok "Using existing image '${LOCAL_IMAGE}'"
    else
        err "Image '${LOCAL_IMAGE}' not found and --skip-build was specified"
        return 1
    fi

    # Use source hash as the base tag (Helm chart appends -haproxy<version>)
    # This makes it easy to verify if running image matches local source
    local base_tag="dev-${source_hash}"
    local unique_tag="${base_tag}-haproxy${HAPROXY_VERSION}"
    local image_with_sha="${LOCAL_IMAGE%%:*}:${unique_tag}"

    debug "Tagging image with source hash: ${unique_tag}"
    docker tag "${LOCAL_IMAGE}" "${image_with_sha}"

    # Load uniquely-tagged image into kind cluster
    run_with_spinner "Loading image '${image_with_sha}' into kind cluster '${CLUSTER_NAME}'" \
        kind load docker-image "${image_with_sha}" --name "${CLUSTER_NAME}"

    # Verify the image was loaded successfully into the cluster
    # Use retry loop to handle timing issues with containerd image indexing
    debug "Verifying image '${image_with_sha}' in kind cluster..."

    local max_attempts=5
    local attempt=1
    local delay=1
    local image_found=false

    while [[ $attempt -le $max_attempts ]]; do
        debug "Verification attempt $attempt/$max_attempts..."

        # Check if crictl can see the image with the specific tag
        # Handle potential registry prefixes (docker.io/library/, localhost/, etc.)
        if docker exec "${CLUSTER_NAME}-control-plane" crictl images 2>/dev/null | grep -q "${LOCAL_IMAGE%%:*}.*${unique_tag}"; then
            image_found=true
            ok "Image verified in cluster"
            break
        fi

        if [[ $attempt -lt $max_attempts ]]; then
            debug "Image not found yet, waiting ${delay}s before retry..."
            sleep "$delay"
            delay=$((delay * 2))  # Exponential backoff: 1s, 2s, 4s, 8s, 16s
        fi

        ((attempt++)) || true
    done

    if [[ "$image_found" != "true" ]]; then
        err "Image not found in cluster after loading and $max_attempts verification attempts"
        warn "Troubleshooting information:"
        echo "  Expected image: ${image_with_sha}"
        echo "  Images currently in cluster:"
        docker exec "${CLUSTER_NAME}-control-plane" crictl images 2>/dev/null | grep "${LOCAL_IMAGE%%:*}" || echo "    (no images matching '${LOCAL_IMAGE%%:*}' found)"
        echo ""
        echo "  Full crictl images output:"
        docker exec "${CLUSTER_NAME}-control-plane" crictl images 2>/dev/null | head -n 20
        return 1
    fi

    # Export base tag for use by Helm (chart appends -haproxy<version>)
    IMAGE_TAG="${base_tag}"
    debug "Exported IMAGE_TAG=${IMAGE_TAG} (full image: ${image_with_sha})"
}

# Populate the global HELM_ARGS array with the shared helm upgrade --install
# arguments. The three positional flags select the call-site-specific options:
#   $1 create_namespace ("true" adds --create-namespace)
#   $2 wait             ("true" adds --wait)
#   $3 reset_values     ("true" adds --reset-values)
# Includes the SPOA_TAG, webhook CA bundle, and Enterprise appends so both
# deploy_controller and dev_restart stay in sync.
build_helm_args() {
    local create_namespace="$1"
    local wait="$2"
    local reset_values="$3"

    HELM_ARGS=(
        "${HELM_RELEASE_NAME}"
        "${REPO_ROOT}/charts/haptic"
        "--namespace" "${CTRL_NAMESPACE}"
    )
    if [[ "$create_namespace" == "true" ]]; then
        HELM_ARGS+=("--create-namespace")
    fi
    HELM_ARGS+=(
        "--values" "${ASSETS_DIR}/dev-values.yaml"
        "--set" "controller.image.tag=${IMAGE_TAG}"
        "--set" "haproxyVersion=${HAPROXY_VERSION}"
        "--set" "haproxy.image.repository=${HAPROXY_REPO}"
        "--set" "haproxy.image.tag=${HAPROXY_TAG}"
    )
    if [[ "$wait" == "true" ]]; then
        HELM_ARGS+=("--wait")
    fi
    HELM_ARGS+=("--timeout" "${TIMEOUT}s")
    if [[ "$reset_values" == "true" ]]; then
        HELM_ARGS+=("--reset-values")
    fi

    # Add Enterprise settings if using Enterprise HAProxy
    # Note: UIDs are automatically derived by the Helm chart based on enterprise.enabled
    if [[ "$HAPROXY_ENTERPRISE" == "true" ]]; then
        HELM_ARGS+=("--set" "haproxy.enterprise.enabled=true")
        debug "Added Enterprise Helm values"
    fi

    # Override the spoa-hub image tag if SPOA_TAG is set (used by CI to
    # point at the freshly-built ci-${CI_PIPELINE_ID} snapshot). Defaults
    # in dev-values.yaml to main-latest for local devs.
    if [[ -n "${SPOA_TAG:-}" ]]; then
        HELM_ARGS+=("--set" "spoaHub.image.tag=${SPOA_TAG}")
        debug "Using SPOA_TAG=${SPOA_TAG} for spoa-hub image"
    fi

    # Add webhook CA bundle if set
    if [[ -n "${WEBHOOK_CA_BUNDLE:-}" ]]; then
        HELM_ARGS+=("--set" "controller.webhook.caBundle=${WEBHOOK_CA_BUNDLE}")
        debug "Added controller.webhook.caBundle to Helm values"
    fi
}

deploy_controller() {
    build_and_load_local_image || {
        err "Failed to build or load image"
        return 1
    }

    print_section "🚀 Deploying Controller via Helm"

    # Create namespace if it doesn't exist
    if ! kubectl get namespace "${CTRL_NAMESPACE}" >/dev/null 2>&1; then
        log INFO "Creating namespace '${CTRL_NAMESPACE}'..."
        kubectl create namespace "${CTRL_NAMESPACE}"

        # Wait for namespace to become active
        debug "Waiting for namespace to become active..."
        local max_wait=30
        local waited=0
        while [[ $waited -lt $max_wait ]]; do
            if kubectl get namespace "${CTRL_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null | grep -q "Active"; then
                ok "Namespace '${CTRL_NAMESPACE}' is active"
                break
            fi
            sleep 1
            ((waited++)) || true
        done

        if [[ $waited -ge $max_wait ]]; then
            warn "Namespace did not become active within ${max_wait}s, continuing anyway..."
        fi
    else
        ok "Using existing namespace '${CTRL_NAMESPACE}'"
    fi

    # Deploy default SSL certificate Secret for HTTPS frontend
    log INFO "Setting up default SSL certificate..."
    if kubectl get secret default-ssl-cert -n "${CTRL_NAMESPACE}" &>/dev/null; then
        ok "Default SSL certificate already exists, skipping generation"
    else
        # Generate self-signed certificate for development
        # Uses generate-dev-ssl-cert.sh which creates a wildcard cert for *.example.com
        log INFO "Generating self-signed certificate for development..."
        "${SCRIPT_DIR}/generate-dev-ssl-cert.sh" "${CTRL_NAMESPACE}" "default-ssl-cert" >/dev/null || {
            err "Failed to generate SSL certificate"
            return 1
        }
        ok "Default SSL certificate generated"
    fi

    # Setup webhook certificates if webhook is enabled
    setup_webhook_certificates || {
        err "Failed to setup webhook certificates"
        return 1
    }

    log INFO "Deploying HAPTIC to namespace '${CTRL_NAMESPACE}' using Helm..."
    log INFO "Using image tag: ${IMAGE_TAG}"
    log INFO "Using HAProxy: ${HAPROXY_REPO}:${HAPROXY_TAG}"

    # Build Helm command with webhook CA bundle if provided.
    # Note: --wait is omitted because readiness probes are disabled in dev mode;
    # we use kubectl rollout status instead to wait for pods.
    build_helm_args false false false

    # Use helm upgrade --install for idempotent deployment
    # Override controller.image.tag with unique SHA-based tag to force pod restart
    if helm upgrade --install "${HELM_ARGS[@]}" 2>&1 | tee /tmp/helm-output.log; then
        ok "Controller Helm release deployed."
    else
        err "Helm deployment failed."
        cat /tmp/helm-output.log
        cleanup_failed_deployment
        return 1
    fi

    # Wait for controller deployment to become ready
    # (readiness probes are disabled in dev mode, so we wait for pods to exist)
    log INFO "Waiting for controller deployment to become ready..."
    if ! kubectl -n "${CTRL_NAMESPACE}" rollout status deployment/${HELM_RELEASE_NAME}-controller --timeout="${TIMEOUT}s"; then
        warn "Controller deployment rollout did not complete in ${TIMEOUT}s."
        echo "  - Check controller deployment status: kubectl -n ${CTRL_NAMESPACE} describe deploy/${HELM_RELEASE_NAME}-controller"
        echo "  - Check controller pod logs: kubectl -n ${CTRL_NAMESPACE} logs deploy/${HELM_RELEASE_NAME}-controller"
        return 1
    fi

    ok "Controller deployed and ready."
}

load_haproxy_enterprise_image() {
    # Only needed for Enterprise images which require authenticated registry
    if [[ "$HAPROXY_ENTERPRISE" != "true" ]]; then
        debug "Using Community HAProxy - no pre-loading needed"
        return 0
    fi

    local image="${HAPROXY_REPO}:${HAPROXY_TAG}"
    log INFO "Pre-loading HAProxy Enterprise image into kind cluster..."

    # Check if image is already in the kind cluster
    if docker exec "${CLUSTER_NAME}-control-plane" crictl images 2>/dev/null | grep -q "${HAPROXY_REPO}.*${HAPROXY_TAG}"; then
        ok "HAProxy Enterprise image already present in cluster"
        return 0
    fi

    # Pull the image locally (requires docker login to hapee-registry.haproxy.com)
    log INFO "Pulling ${image} from registry..."
    if ! docker pull "${image}" 2>&1; then
        err "Failed to pull ${image}"
        warn "Ensure you are logged in to the HAProxy Enterprise registry:"
        echo "  docker login hapee-registry.haproxy.com"
        return 1
    fi
    ok "Image pulled successfully"

    # Load into kind cluster
    run_with_spinner "Loading ${image} into kind cluster" \
        kind load docker-image "${image}" --name "${CLUSTER_NAME}" || {
        err "Failed to load image into kind cluster"
        return 1
    }

    ok "HAProxy Enterprise image loaded into cluster"
}

deploy_haproxy() {
    print_section "🔧 Verifying HAProxy Deployment"

    # Pre-load Enterprise image if needed (Community images are pulled directly)
    load_haproxy_enterprise_image || {
        err "Failed to load HAProxy Enterprise image"
        return 1
    }

    log INFO "Waiting for HAProxy deployment to become ready (${HAPROXY_REPO}:${HAPROXY_TAG})..."
    if ! kubectl -n "${CTRL_NAMESPACE}" rollout status deployment/${HELM_RELEASE_NAME}-haproxy --timeout="${TIMEOUT}s"; then
        warn "HAProxy deployment rollout did not complete in ${TIMEOUT}s."
        echo "  - Check HAProxy deployment: kubectl -n ${CTRL_NAMESPACE} describe deploy/${HELM_RELEASE_NAME}-haproxy"
        echo "  - Check HAProxy pod logs: kubectl -n ${CTRL_NAMESPACE} logs deploy/${HELM_RELEASE_NAME}-haproxy -c haproxy"
        echo "  - Check agent logs: kubectl -n ${CTRL_NAMESPACE} logs deploy/${HELM_RELEASE_NAME}-haproxy -c agent"
        return 1
    fi
    ok "HAProxy deployment is ready."
}

deploy_gateway_demo() {
	log INFO "Deploying Gateway API demo resources..."

	# Deploy Gateway API demo resources (CRDs already installed in ensure_cluster)
	log INFO "Applying Gateway API demo manifests..."
	retry_with_backoff 3 2 kubectl apply -f "${ASSETS_DIR}/gateway-demo.yaml" || {
		err "Failed to deploy Gateway API demo resources"
		return 1
	}

	# Wait for echo-server-v2 deployment
	log INFO "Waiting for echo-server-v2 deployment to become ready..."
	if ! kubectl -n "${ECHO_NAMESPACE}" rollout status deployment/echo-server-v2 --timeout=120s >/dev/null 2>&1; then
		warn "echo-server-v2 deployment did not become ready in time"
		return 1
	fi

	ok "Gateway API demo resources deployed"
	echo "Gateway: dev-gateway"
	echo "HTTPRoutes:"
	echo "  - echo-basic (echo-gateway.localdev.me) - Basic routing"
	echo "  - echo-paths (echo-paths.localdev.me) - Path matching variants"
	echo "  - echo-split (echo-split.localdev.me) - Traffic splitting 70/30"
	echo "  - echo-methods (echo-methods.localdev.me) - HTTP method matching"
	echo "  - echo-headers (echo-headers.localdev.me) - Header matching"
	echo "  - echo-query (echo-query.localdev.me) - Query parameter matching"
	echo "  - echo-combined (echo-combined.localdev.me) - Combined matchers"
}

deploy_echo_server() {
	log INFO "Creating demo Echo Server in namespace '${ECHO_NAMESPACE}'..."
	kubectl get ns "${ECHO_NAMESPACE}" >/dev/null 2>&1 || kubectl create ns "${ECHO_NAMESPACE}" >/dev/null

	# Deploy echo-server application and service
	kubectl -n "${ECHO_NAMESPACE}" apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${ECHO_APP_NAME}
  labels:
    app: ${ECHO_APP_NAME}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${ECHO_APP_NAME}
  template:
    metadata:
      labels:
        app: ${ECHO_APP_NAME}
    spec:
      containers:
        - name: server
          image: ${ECHO_IMAGE}
          imagePullPolicy: IfNotPresent
          ports:
            - containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: ${ECHO_APP_NAME}
  labels:
    app: ${ECHO_APP_NAME}
spec:
  selector:
    app: ${ECHO_APP_NAME}
  ports:
    - name: http
      port: 80
      targetPort: 80
EOF

	log INFO "Waiting for Echo Server deployment to become ready..."
	kubectl -n "${ECHO_NAMESPACE}" rollout status deployment/${ECHO_APP_NAME} --timeout=120s >/dev/null
	ok "Echo Server is ready."
}

# Deploy blocklist-server early so controller templates can fetch from it
# This must be called BEFORE deploy_controller when not using --skip-echo
deploy_blocklist_server() {
	log INFO "Deploying blocklist-server for HTTP Store demo..."

	# Create echo namespace if it doesn't exist
	kubectl get ns "${ECHO_NAMESPACE}" >/dev/null 2>&1 || kubectl create ns "${ECHO_NAMESPACE}" >/dev/null

	# Deploy blocklist-server
	kubectl apply -f "${ASSETS_DIR}/blocklist-server.yaml" >/dev/null || {
		err "Failed to deploy blocklist server"
		return 1
	}

	# Wait for deployment to be ready
	log INFO "Waiting for blocklist-server to become ready..."
	if ! kubectl -n "${ECHO_NAMESPACE}" rollout status deployment/blocklist-server --timeout=60s >/dev/null 2>&1; then
		warn "blocklist-server deployment did not become ready in time, controller startup may fail"
		return 0
	fi

	ok "Blocklist server is ready."
}

# Deploy auth-server fixture (used by the external-auth demo ingresses).
# Must be running before HAProxy can serve the auth-allowed/auth-denied
# routes — the spoa-hub external-auth plugin calls into it at request time.
deploy_auth_server() {
	log INFO "Deploying auth-server for external-auth demo..."

	kubectl get ns "${ECHO_NAMESPACE}" >/dev/null 2>&1 || kubectl create ns "${ECHO_NAMESPACE}" >/dev/null

	kubectl apply -f "${ASSETS_DIR}/auth-server.yaml" >/dev/null || {
		err "Failed to deploy auth server"
		return 1
	}

	log INFO "Waiting for auth-server to become ready..."
	if ! kubectl -n "${ECHO_NAMESPACE}" rollout status deployment/auth-server --timeout=60s >/dev/null 2>&1; then
		warn "auth-server deployment did not become ready in time"
		return 0
	fi

	ok "Auth server is ready."
}

# Generate test certificates and create the Secrets the auth-tls
# E2E demo ingress depends on. Runs the openssl chain for:
#   - a self-signed CA + matching server cert (CN auth-tls.localdev.me)
#   - a client cert signed by that CA (the "valid client" for the test)
#   - a separate untrusted CA + a client cert it signed (the "wrong
#     client" — sent by curl to verify the rejection path)
#
# Server cert lands in `auth-tls-server-tls` (kubernetes.io/tls);
# the trusted CA bundle lands in `auth-tls-client-ca` as Opaque
# with a single `ca.crt` field, which is what nginx-ingress and
# haproxy-ingress operators conventionally use (the chart only
# reads `data.ca.crt` regardless of secret type).
#
# Cert files stay on disk at /tmp/haptic-auth-tls-test/ for ad-hoc
# curl-based testing (the Go e2e suite under tests/e2e/ generates its
# own per-test certs).
deploy_auth_tls_fixtures() {
	local cert_dir="${TMPDIR:-/tmp}/haptic-auth-tls-test"
	log INFO "Generating auth-tls E2E certificates in ${cert_dir}..."
	rm -rf "$cert_dir"
	mkdir -p "$cert_dir"
	chmod 700 "$cert_dir"

	# Trusted CA + server cert (signed by it).
	openssl req -x509 -newkey rsa:2048 -nodes \
		-keyout "$cert_dir/ca.key" -out "$cert_dir/ca.crt" \
		-days 36500 -subj "/CN=HAPTIC E2E Trusted CA" 2>/dev/null

	openssl req -newkey rsa:2048 -nodes \
		-keyout "$cert_dir/server.key" -out "$cert_dir/server.csr" \
		-subj "/CN=auth-tls.localdev.me" 2>/dev/null
	cat > "$cert_dir/server.ext" <<-EOF
		authorityKeyIdentifier=keyid,issuer
		basicConstraints=CA:FALSE
		subjectAltName = @alt_names
		[alt_names]
		DNS.1 = auth-tls.localdev.me
	EOF
	openssl x509 -req -in "$cert_dir/server.csr" \
		-CA "$cert_dir/ca.crt" -CAkey "$cert_dir/ca.key" -CAcreateserial \
		-out "$cert_dir/server.crt" -days 36500 \
		-extfile "$cert_dir/server.ext" 2>/dev/null

	# Valid client cert (signed by trusted CA → HAProxy will accept).
	openssl req -newkey rsa:2048 -nodes \
		-keyout "$cert_dir/client.key" -out "$cert_dir/client.csr" \
		-subj "/CN=test-client" 2>/dev/null
	openssl x509 -req -in "$cert_dir/client.csr" \
		-CA "$cert_dir/ca.crt" -CAkey "$cert_dir/ca.key" -CAcreateserial \
		-out "$cert_dir/client.crt" -days 36500 2>/dev/null

	# Untrusted CA + a client cert it signed (HAProxy will reject —
	# the trusted CA bundle in the K8s Secret doesn't contain this CA).
	openssl req -x509 -newkey rsa:2048 -nodes \
		-keyout "$cert_dir/wrong-ca.key" -out "$cert_dir/wrong-ca.crt" \
		-days 36500 -subj "/CN=HAPTIC E2E Untrusted CA" 2>/dev/null
	openssl req -newkey rsa:2048 -nodes \
		-keyout "$cert_dir/wrong-client.key" -out "$cert_dir/wrong-client.csr" \
		-subj "/CN=untrusted-client" 2>/dev/null
	openssl x509 -req -in "$cert_dir/wrong-client.csr" \
		-CA "$cert_dir/wrong-ca.crt" -CAkey "$cert_dir/wrong-ca.key" -CAcreateserial \
		-out "$cert_dir/wrong-client.crt" -days 36500 2>/dev/null

	log INFO "Creating K8s Secrets for auth-tls fixtures..."
	kubectl get ns "${ECHO_NAMESPACE}" >/dev/null 2>&1 || \
		kubectl create ns "${ECHO_NAMESPACE}" >/dev/null

	kubectl -n "${ECHO_NAMESPACE}" create secret tls auth-tls-server-tls \
		--cert="$cert_dir/server.crt" --key="$cert_dir/server.key" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null

	# Trusted CA bundle. Use Opaque (not kubernetes.io/tls) — the K8s
	# API enforces tls.crt + tls.key presence on TLS-typed Secrets,
	# and we only want to ship a CA bundle here. The chart's snippet
	# reads `data.ca.crt` regardless of secret type.
	kubectl -n "${ECHO_NAMESPACE}" create secret generic auth-tls-client-ca \
		--from-file=ca.crt="$cert_dir/ca.crt" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null

	ok "Auth-TLS fixtures deployed (cert files at ${cert_dir})"
}

deploy_ingress_demo() {
	log INFO "Deploying Ingress demo resources..."

	# Deploy demo backend for PROXY protocol, SSL, and SSL passthrough testing
	kubectl apply -f "${ASSETS_DIR}/haproxy-demo-backend.yaml" >/dev/null || {
		err "Failed to deploy demo backend"
		return 1
	}

	# Note: blocklist-server is deployed earlier by deploy_blocklist_server()
	# to ensure it's ready before controller starts template rendering

	# Deploy comprehensive Ingress examples from ingress-demo.yaml
	# This file demonstrates all supported haproxy.org/* annotations
	kubectl apply -f "${ASSETS_DIR}/ingress-demo.yaml" >/dev/null || {
		err "Failed to deploy Ingress demo resources"
		return 1
	}

	ok "Ingress demo resources deployed"

	# List all deployed Ingress resources
	echo "Ingress resources (demonstrating haproxy.org/* annotations):"
	echo "  Basic:"
	echo "    - echo-basic (echo.localdev.me) - No annotations baseline"
	echo "    - echo-auth (echo-auth.localdev.me) - Basic authentication"
	echo ""
	echo "  CORS & Access Control:"
	echo "    - echo-cors (echo-cors.localdev.me) - CORS headers"
	echo "    - echo-allowlist (echo-allowlist.localdev.me) - IP allowlist"
	echo "    - echo-denylist (echo-denylist.localdev.me) - IP denylist"
	echo ""
	echo "  Traffic Management:"
	echo "    - echo-ratelimit (echo-ratelimit.localdev.me) - Rate limiting"
	echo "    - echo-loadbalance (echo-loadbalance.localdev.me) - Load balancing algorithm"
	echo "    - echo-sticky (echo-sticky.localdev.me) - Sticky sessions"
	echo "    - echo-maxconn (echo-maxconn.localdev.me) - Connection limits"
	echo ""
	echo "  Redirects:"
	echo "    - echo-ssl-redirect (echo-ssl-redirect.localdev.me) - SSL redirect"
	echo "    - echo-redirect (echo-redirect.localdev.me) - Request redirect"
	echo ""
	echo "  Headers & Routing:"
	echo "    - echo-headers-request (echo-headers-request.localdev.me) - Request headers"
	echo "    - echo-headers-response (echo-headers-response.localdev.me) - Response headers"
	echo "    - echo-sethost (echo-sethost.localdev.me) - Host header override"
	echo "    - echo-rewrite (echo-rewrite.localdev.me) - Path rewriting"
	echo "    - echo-forwardedfor (echo-forwardedfor.localdev.me) - X-Forwarded-For"
	echo ""
	echo "  Backend Configuration:"
	echo "    - echo-timeouts (echo-timeouts.localdev.me) - Custom timeouts"
	echo "    - echo-healthcheck (echo-healthcheck.localdev.me) - Health checks"
	echo "    - echo-capture (echo-capture.localdev.me) - Request capture"
	echo "    - echo-srcip (echo-srcip.localdev.me) - Source IP header"
	echo "    - echo-proxy-protocol (echo-proxy-protocol.localdev.me) - PROXY protocol to backends"
	echo "    - echo-backend-ssl (echo-backend-ssl.localdev.me) - SSL/TLS + HTTP/2 to backends"
	echo "    - echo-backend-snippet (echo-backend-snippet.localdev.me) - Raw HAProxy config"
	echo ""
	echo "  Combined:"
	echo "    - echo-combined (echo-combined.localdev.me) - Multiple annotations"
	echo ""
	echo "Run 'make test-e2e' to test all examples (full Go e2e suite under tests/e2e/)"
}

# Development convenience functions
dev_logs() {
    verify_cluster_context
    # Use label selector to find controller deployment
    kubectl -n "$CTRL_NAMESPACE" logs -f -l "app.kubernetes.io/instance=${HELM_RELEASE_NAME},app.kubernetes.io/name=haptic,app.kubernetes.io/component=controller"
}

dev_exec() {
    verify_cluster_context
    # Get the deployment name using label selector
    local deploy_name
    deploy_name=$(kubectl -n "$CTRL_NAMESPACE" get deployment -l "app.kubernetes.io/instance=${HELM_RELEASE_NAME},app.kubernetes.io/name=haptic,app.kubernetes.io/component=controller" -o jsonpath='{.items[0].metadata.name}')
    kubectl -n "$CTRL_NAMESPACE" exec -it deploy/"$deploy_name" -- sh
}

dev_restart() {
    verify_cluster_context
    print_section "🔄 Restarting Controller"

    # Build and load new image unless skipped.
    # Even when --skip-build is set we still call build_and_load_local_image to
    # tag and load the existing image with a unique tag.
    if [[ "$SKIP_BUILD" != "true" ]]; then
        log INFO "Rebuilding and loading controller image..."
    else
        log INFO "Skipping image rebuild (--skip-build flag set)"
    fi
    build_and_load_local_image || {
        err "Failed to rebuild image"
        return 1
    }

    # Setup webhook certificates if webhook is enabled
    setup_webhook_certificates || {
        err "Failed to setup webhook certificates"
        return 1
    }

    # Build Helm command with webhook CA bundle if provided
    build_helm_args true true true

    # Upgrade Helm release to pick up any changes
    # Using --install makes this idempotent (works even if release doesn't exist)
    # Override controller.image.tag with unique SHA-based tag to force pod restart
    log INFO "Upgrading Helm release with image tag: ${IMAGE_TAG}..."
    log INFO "Using HAProxy: ${HAPROXY_REPO}:${HAPROXY_TAG}"
    helm upgrade --install "${HELM_ARGS[@]}" || {
        warn "Helm upgrade failed, rolling back..."
        helm rollback "${HELM_RELEASE_NAME}" -n "${CTRL_NAMESPACE}" 2>/dev/null || true
        return 1
    }

    ok "Controller restarted successfully"
}

dev_status() {
    verify_cluster_context
    print_section "📊 Development Environment Status"

    # Source hash verification (most important - show first)
    echo "Source Code Sync:"
    local local_hash running_hash
    local_hash=$("${SCRIPT_DIR}/source-hash.sh" 2>/dev/null || echo "error")

    # Get running source hash from controller startup logs (appears in first few lines)
    # Use deployment/ instead of -l selector to avoid interleaved multi-pod logs
    running_hash=$(kubectl -n "$CTRL_NAMESPACE" logs "deployment/${HELM_RELEASE_NAME}-controller" 2>/dev/null | \
        head -20 | grep "source_hash=" | head -1 | sed 's/.*source_hash=\([a-f0-9]*\).*/\1/' || echo "")

    if [[ -z "$running_hash" ]]; then
        running_hash="unknown"
    fi

    echo "  Local source hash:   ${local_hash}"
    echo "  Running source hash: ${running_hash}"

    if [[ "$local_hash" == "$running_hash" ]]; then
        ok "IN SYNC - dev environment is running current code"
    elif [[ "$running_hash" == "unknown" ]]; then
        warn "UNKNOWN - could not determine running source hash (check controller logs)"
    else
        err "OUT OF SYNC - run './scripts/start-dev-env.sh restart' to update"
    fi

    echo
    echo "Cluster:"
    if kind get clusters 2>/dev/null | grep -q "$CLUSTER_NAME"; then
        ok "Cluster '$CLUSTER_NAME' exists"
        kubectl cluster-info | head -n 3
    else
        err "Cluster '$CLUSTER_NAME' not found"
        return 1
    fi

    echo
    echo "Helm Releases:"
    helm list -n "$CTRL_NAMESPACE" 2>/dev/null || {
        warn "Namespace '$CTRL_NAMESPACE' not found or no releases"
    }

    echo
    echo "Deployments:"
    kubectl -n "$CTRL_NAMESPACE" get deploy -o wide 2>/dev/null || {
        warn "Namespace '$CTRL_NAMESPACE' not found or no deployments"
    }

    echo
    echo "Pods:"
    kubectl -n "$CTRL_NAMESPACE" get pods -o wide 2>/dev/null || {
        warn "No pods found in namespace '$CTRL_NAMESPACE'"
    }

    echo
    echo "Services:"
    kubectl -n "$CTRL_NAMESPACE" get svc 2>/dev/null || {
        warn "No services found in namespace '$CTRL_NAMESPACE'"
    }
}

dev_clean() {
    print_section "🧹 Cleaning Development Environment"

    if kind get clusters 2>/dev/null | grep -q "$CLUSTER_NAME"; then
        log INFO "Deleting kind cluster '$CLUSTER_NAME'..."
        kind delete cluster --name "$CLUSTER_NAME"
        ok "Cluster deleted"
    else
        warn "Cluster '$CLUSTER_NAME' not found"
    fi
}

dev_down() {
    dev_clean "$@"
}

test_ingress() {
    verify_cluster_context
    print_section "🧪 Testing Ingress Controller"

    # Check if the echo service is available
    if ! kubectl -n "$ECHO_NAMESPACE" get deployment "$ECHO_APP_NAME" >/dev/null 2>&1; then
        warn "Echo service not found. Deploy it first with: $0 up"
        return 1
    fi

    # Test via NodePort (requires kind cluster)
    if kind get clusters 2>/dev/null | grep -q "$CLUSTER_NAME"; then
        log INFO "Testing via NodePort (${NODEPORT_HOST}:30080)..."
        echo "Trying to connect to echo service through HAProxy ingress controller..."

        # Wait for the service to be ready with retries
        local max_attempts=10
        local delay=2
        local attempt=1
        local test_successful=false

        while [[ $attempt -le $max_attempts ]]; do
            debug "Connection attempt $attempt/$max_attempts..."

            if curl -s --max-time 5 -H "Host: echo.localdev.me" "http://${NODEPORT_HOST}:30080" >/dev/null 2>&1; then
                test_successful=true
                break
            fi

            if [[ $attempt -lt $max_attempts ]]; then
                debug "Connection failed, waiting ${delay}s before retry..."
                sleep "$delay"
            fi

            ((attempt++)) || true
        done

        if [[ "$test_successful" == "true" ]]; then
            ok "Ingress controller is working! Echo service is accessible"
            echo
            echo "Test the ingress controller with:"
            echo "  curl -H 'Host: echo.localdev.me' http://${NODEPORT_HOST}:30080"
            echo "  curl -H 'Host: echo.localdev.me' http://${NODEPORT_HOST}:30080/test"
            echo
            echo "Or in your browser:"
            echo "  Add '127.0.0.1 echo.localdev.me' to /etc/hosts"
            echo "  Then visit: http://echo.localdev.me:30080"
        else
            warn "Could not reach echo service through ingress controller after $max_attempts attempts"
            echo
            echo "Troubleshooting steps:"
            echo "  1. Check HAProxy status: kubectl -n $CTRL_NAMESPACE get pods -l app.kubernetes.io/component=loadbalancer"
            echo "  2. Check HAProxy logs: kubectl -n $CTRL_NAMESPACE logs deploy/haproxy-production -c haproxy"
            echo "  3. Check agent logs: kubectl -n $CTRL_NAMESPACE logs deploy/haproxy-production -c agent"
            echo "  4. Check controller logs: $0 logs"
            echo "  5. Check echo service: kubectl -n $ECHO_NAMESPACE get pods -l app=$ECHO_APP_NAME"
            echo "  6. Verify ingress: kubectl -n $ECHO_NAMESPACE get ingress $ECHO_APP_NAME -o wide"
            echo "  7. Check HAProxy config: kubectl -n $CTRL_NAMESPACE exec deploy/haproxy-production -c haproxy -- cat /etc/haproxy/haproxy.cfg"
            return 1
        fi
    else
        warn "Kind cluster '$CLUSTER_NAME' not found. Cannot test via NodePort."
        return 1
    fi
}

port_forward_haproxy() {
    verify_cluster_context
    print_section "🔄 Setting up Port Forwarding"

    echo "Setting up port forwarding for HAProxy services..."
    echo "This will forward local ports to the HAProxy services in the cluster."
    echo

    # Check if namespace exists
    if ! kubectl get namespace "$CTRL_NAMESPACE" >/dev/null 2>&1; then
        err "Namespace '$CTRL_NAMESPACE' not found. Deploy the environment first with: $0 up"
        return 1
    fi

    # Check if HAProxy is running
    if ! kubectl -n "$CTRL_NAMESPACE" get deployment haproxy-production >/dev/null 2>&1; then
        warn "HAProxy production deployment not found. Deploy it first with: $0 up"
        return 1
    fi

    # Verify HAProxy deployment is ready
    local haproxy_ready
    haproxy_ready=$(kubectl -n "$CTRL_NAMESPACE" get deployment haproxy-production -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    if [[ "$haproxy_ready" == "0" ]]; then
        warn "HAProxy deployment exists but has no ready pods"
        echo "Check deployment status with: kubectl -n $CTRL_NAMESPACE get deploy/haproxy-production"
        return 1
    fi

    # Get controller deployment name and verify it exists
    local ctrl_deploy
    ctrl_deploy=$(kubectl -n "$CTRL_NAMESPACE" get deployment -l "app.kubernetes.io/instance=${HELM_RELEASE_NAME},app.kubernetes.io/name=haptic,app.kubernetes.io/component=controller" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

    if [[ -z "$ctrl_deploy" ]]; then
        warn "Controller deployment not found"
        ctrl_deploy="(not available)"
    fi

    echo "Available diagnostics:"
    echo "  1. HAProxy HTTP (port 8080 -> 80)"
    echo "  2. HAProxy Health (port 8404 -> 8404)"
    echo "  3. HAPTIC agent state"

    if [[ "$ctrl_deploy" != "(not available)" ]]; then
        echo "  4. Controller Health (port 8081 -> 8080)"
        echo "  5. Controller Metrics (port 9090 -> 9090)"
    else
        echo "  4. Controller Health - ${YELLOW}(not available)${NC}"
        echo "  5. Controller Metrics - ${YELLOW}(not available)${NC}"
    fi

    echo

    read -p "Choose an option (1-5) or 'q' to quit: " choice

    case "$choice" in
        1)
            log INFO "Starting port forwarding: localhost:8080 -> HAProxy HTTP"
            echo "Test with: curl -H 'Host: echo.localdev.me' http://localhost:8080"
            kubectl -n "$CTRL_NAMESPACE" port-forward svc/haproxy-production 8080:80
            ;;
        2)
            log INFO "Starting port forwarding: localhost:8404 -> HAProxy Health"
            echo "Test with: curl http://localhost:8404/healthz"
            kubectl -n "$CTRL_NAMESPACE" port-forward svc/haproxy-production 8404:8404
            ;;
        3)
            kubectl -n "$CTRL_NAMESPACE" exec deployment/haproxy-production -c agent -- haptic agent state
            ;;
        4)
            if [[ "$ctrl_deploy" == "(not available)" ]]; then
                err "Controller deployment not found. Deploy it first with: $0 up"
                return 1
            fi
            log INFO "Starting port forwarding: localhost:8081 -> Controller Health"
            echo "Test with: curl http://localhost:8081/healthz"
            kubectl -n "$CTRL_NAMESPACE" port-forward deploy/${ctrl_deploy} 8081:8080
            ;;
        5)
            if [[ "$ctrl_deploy" == "(not available)" ]]; then
                err "Controller deployment not found. Deploy it first with: $0 up"
                return 1
            fi
            log INFO "Starting port forwarding: localhost:9090 -> Controller Metrics"
            echo "Test with: curl http://localhost:9090/metrics"
            kubectl -n "$CTRL_NAMESPACE" port-forward deploy/${ctrl_deploy} 9090:9090
            ;;
        q|Q)
            echo "Exiting port forwarding setup"
            return 0
            ;;
        *)
            err "Invalid choice: $choice"
            return 1
            ;;
    esac
}

post_deploy_tips() {
	echo
	ok "Environment is up. Next steps:"
	echo "  - Watch resources: kubectl get pods -A -w"
	echo "  - Controller logs: $0 logs"
	echo "  - HAProxy production logs: kubectl -n ${CTRL_NAMESPACE} logs deploy/haproxy-production -f -c haproxy"
	echo "  - Agent logs: kubectl -n ${CTRL_NAMESPACE} logs deploy/haproxy-production -f -c agent"
	echo "  - Echo service: kubectl -n ${ECHO_NAMESPACE} get svc ${ECHO_APP_NAME}"
	echo "  - Ingress: kubectl -n ${ECHO_NAMESPACE} get ingress ${ECHO_APP_NAME} -o wide"
	echo

	ok "🧪 Testing the Ingress Controller:"
	echo "  - Quick test: $0 test"
	echo
	echo "  Ingress resources (networking.k8s.io/v1):"
	echo "    Public endpoint (no auth):"
	echo "      curl -H 'Host: echo.localdev.me' http://localhost:30080"
	echo
	echo "    Protected endpoint (basic auth):"
	echo "      curl -H 'Host: echo-auth.localdev.me' http://localhost:30080  # Returns 401"
	echo "      curl -u admin:admin -H 'Host: echo-auth.localdev.me' http://localhost:30080  # Succeeds"
	echo "      curl -u user:password -H 'Host: echo-auth.localdev.me' http://localhost:30080  # Succeeds"
	echo
	echo "  HTTPRoute resources (gateway.networking.k8s.io/v1):"
	echo "    Basic routing:"
	echo "      curl -H 'Host: echo-gateway.localdev.me' http://localhost:30080"
	echo
	echo "    Path matching variants:"
	echo "      curl -H 'Host: echo-paths.localdev.me' http://localhost:30080/exact  # Exact match"
	echo "      curl -H 'Host: echo-paths.localdev.me' http://localhost:30080/api/v1  # PathPrefix match"
	echo
	echo "    Traffic splitting (70% v1 / 30% v2):"
	echo "      for i in {1..10}; do curl -s -H 'Host: echo-split.localdev.me' http://localhost:30080 | grep -o 'ENVIRONMENT.*'; done"
	echo
	echo "    HTTP method matching:"
	echo "      curl -X GET -H 'Host: echo-methods.localdev.me' http://localhost:30080/api  # Routes to v2"
	echo "      curl -X POST -H 'Host: echo-methods.localdev.me' http://localhost:30080/api  # Routes to v1"
	echo
	echo "    Header matching:"
	echo "      curl -H 'Host: echo-headers.localdev.me' -H 'X-Api-Version: v2' http://localhost:30080  # Routes to v2"
	echo "      curl -H 'Host: echo-headers.localdev.me' -H 'X-User-Agent: mobile-app-ios' http://localhost:30080  # Routes to v2"
	echo
	echo "    Query parameter matching:"
	echo "      curl -H 'Host: echo-query.localdev.me' 'http://localhost:30080/search?version=beta'  # Routes to v2"
	echo "      curl -H 'Host: echo-query.localdev.me' 'http://localhost:30080/search?debug=true'  # Routes to v2"
	echo
	echo "    Combined matchers (method + headers + query):"
	echo "      curl -X POST -H 'Host: echo-combined.localdev.me' -H 'Content-Type: application/json' 'http://localhost:30080/api?token=secret123'"
	echo
	echo "  CORS example:"
	echo "    curl -H 'Host: echo-cors.localdev.me' -H 'Origin: https://example.com' http://localhost:30080"
	echo
	echo "  Rate limiting example:"
	echo "    for i in {1..10}; do curl -s -H 'Host: echo-ratelimit.localdev.me' http://localhost:30080 && echo; done  # Exceeds limit after 5 requests"
	echo
	echo "  Redirect example:"
	echo "    curl -v -H 'Host: echo-redirect.localdev.me' http://localhost:30080  # Returns 302 redirect"
	echo
	echo "  Header manipulation example:"
	echo "    curl -H 'Host: echo-headers-response.localdev.me' http://localhost:30080 -I  # See response headers"
	echo
	echo "  Path rewrite example:"
	echo "    curl -H 'Host: echo-rewrite.localdev.me' http://localhost:30080/api/v1/test  # Rewrites to /test"
	echo
	echo "  SSL redirect example:"
	echo "    curl -v -H 'Host: echo-ssl-redirect.localdev.me' http://localhost:30080  # Returns 301 redirect to HTTPS"
	echo
	echo "  Backend SSL/HTTP2 example:"
	echo "    curl -H 'Host: echo-backend-ssl.localdev.me' http://localhost:30080  # Uses HTTPS + HTTP/2 to backend (if backend supports it)"
	echo
	echo "  Browser test:"
	echo "    Add to /etc/hosts:"
	echo "      127.0.0.1 echo.localdev.me echo-auth.localdev.me echo-cors.localdev.me echo-ratelimit.localdev.me echo-allowlist.localdev.me echo-denylist.localdev.me echo-ssl.localdev.me echo-redirect.localdev.me echo-headers-request.localdev.me echo-headers-response.localdev.me echo-sethost.localdev.me echo-rewrite.localdev.me echo-loadbalance.localdev.me echo-sticky.localdev.me echo-timeouts.localdev.me echo-forwardedfor.localdev.me echo-capture.localdev.me echo-healthcheck.localdev.me echo-maxconn.localdev.me echo-srcip.localdev.me echo-backend-snippet.localdev.me echo-ssl-redirect.localdev.me echo-proxy-protocol.localdev.me echo-backend-ssl.localdev.me echo-combined.localdev.me echo-gateway.localdev.me echo-paths.localdev.me echo-split.localdev.me echo-methods.localdev.me echo-headers.localdev.me echo-query.localdev.me"
	echo "    Visit: http://echo.localdev.me:30080 (Ingress, no auth)"
	echo "    Visit: http://echo-auth.localdev.me:30080 (Ingress, basic auth)"
	echo "    Visit: http://echo-cors.localdev.me:30080 (Ingress, CORS)"
	echo "    Visit: http://echo-ratelimit.localdev.me:30080 (Ingress, rate limiting)"
	echo "    Visit: http://echo-gateway.localdev.me:30080 (HTTPRoute, basic)"
	echo "    Visit: http://echo-split.localdev.me:30080 (HTTPRoute, traffic split - refresh multiple times)"
	echo
	echo "  - Port forwarding: $0 port-forward"
	echo

	ok "🎛️  HAProxy Production Environment:"
	echo "  - Production HAProxy pods: kubectl -n ${CTRL_NAMESPACE} get deploy/haproxy-production"
	echo "  - HAProxy service: kubectl -n ${CTRL_NAMESPACE} get svc/haproxy-production"
	echo "  - Agent service: kubectl -n ${CTRL_NAMESPACE} get svc/haproxy-production-dataplane"
	echo "  - Agent state: kubectl -n ${CTRL_NAMESPACE} exec deployment/haproxy-production -c agent -- haptic agent state"
	echo

	ok "📊 Monitoring:"
	echo "  - Controller health: curl http://localhost:30080/healthz (via port-forward)"
	echo "  - Controller metrics: curl http://localhost:9090/metrics (via port-forward)"
	echo "  - HAProxy health: curl http://localhost:30404/healthz (via NodePort)"
	echo

	warn "Troubleshooting hints:"
	echo "  - Check status: $0 status"
	echo "  - Inspect events: kubectl get events --all-namespaces --sort-by=.lastTimestamp | tail -n 50"
	echo "  - Describe controller: kubectl -n ${CTRL_NAMESPACE} get deployments -l app.kubernetes.io/instance=${HELM_RELEASE_NAME}"
	echo "  - Describe HAProxy: kubectl -n ${CTRL_NAMESPACE} describe deploy/haproxy-production"
	echo "  - Check Helm release: helm list -n ${CTRL_NAMESPACE}"
	echo "  - Helm history: helm history ${HELM_RELEASE_NAME} -n ${CTRL_NAMESPACE}"
	echo
}

dev_metrics() {
	verify_cluster_context

	# Use global flags set by parse_args
	local show_raw="$METRICS_RAW"
	local watch_mode="$METRICS_WATCH"

	print_section "📊 Controller Metrics"

	# Get controller deployment
	local ctrl_deploy
	ctrl_deploy=$(kubectl -n "$CTRL_NAMESPACE" get deployment -l "app.kubernetes.io/instance=${HELM_RELEASE_NAME},app.kubernetes.io/name=haptic,app.kubernetes.io/component=controller" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

	if [[ -z "$ctrl_deploy" ]]; then
		err "Controller deployment not found in namespace '$CTRL_NAMESPACE'"
		warn "Deploy the controller first with: $0 up"
		return 1
	fi

	# Check if controller pod is ready
	local pod_ready
	pod_ready=$(kubectl -n "$CTRL_NAMESPACE" get deployment "$ctrl_deploy" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
	if [[ "$pod_ready" == "0" ]]; then
		err "Controller deployment exists but has no ready pods"
		echo "Check status with: $0 status"
		return 1
	fi

	ok "Controller is running"

	# Set up port-forward in background
	log INFO "Setting up port-forward to metrics endpoint (port 9090)..."

	# Use a temporary port-forward that we'll clean up
	local pf_pid
	kubectl -n "$CTRL_NAMESPACE" port-forward deploy/"$ctrl_deploy" 9090:9090 >/dev/null 2>&1 &
	pf_pid=$!

	# Ensure cleanup on exit
	trap "kill $pf_pid 2>/dev/null || true" RETURN

	# Wait for port-forward to be ready
	local max_wait=10
	local waited=0
	while [[ $waited -lt $max_wait ]]; do
		if curl -s --max-time 1 http://localhost:9090/metrics >/dev/null 2>&1; then
			break
		fi
		sleep 0.5
		((waited++)) || true
	done

	if [[ $waited -ge $max_wait ]]; then
		err "Port-forward did not become ready in time"
		return 1
	fi

	ok "Port-forward ready"
	echo

	if [[ "$watch_mode" == "true" ]]; then
		log INFO "Starting watch mode (Ctrl+C to exit)..."
		echo "Refreshing every 2 seconds..."
		echo

		# Use watch command if available, otherwise loop
		if command -v watch >/dev/null 2>&1; then
			watch -n 2 "curl -s http://localhost:9090/metrics 2>/dev/null | grep -E '^haproxy_ic_' | grep -v '#'"
		else
			while true; do
				clear
				echo "$(date) - Controller Metrics (refreshing every 2s)"
				echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
				curl -s http://localhost:9090/metrics 2>/dev/null | grep -E '^haproxy_ic_' | grep -v '#'
				sleep 2
			done
		fi
	elif [[ "$show_raw" == "true" ]]; then
		log INFO "Fetching raw metrics..."
		curl -s http://localhost:9090/metrics
	else
		log INFO "Fetching controller metrics..."

		local metrics_output
		metrics_output=$(curl -s http://localhost:9090/metrics 2>/dev/null)

		if [[ -z "$metrics_output" ]]; then
			err "Failed to fetch metrics"
			return 1
		fi

		echo -e "${GREEN}Operational Metrics:${NC}"
		echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

		# Extract key metrics
		echo "$metrics_output" | grep -E '^haproxy_ic_(reconciliation|deployment|validation)_total' | sed 's/haproxy_ic_/  /'

		echo
		echo -e "${YELLOW}Error Metrics:${NC}"
		echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
		echo "$metrics_output" | grep -E '^haproxy_ic_.*_errors_total' | sed 's/haproxy_ic_/  /'

		echo
		echo -e "${BLUE}Resource Metrics:${NC}"
		echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
		echo "$metrics_output" | grep -E '^haproxy_ic_resource_count' | sed 's/haproxy_ic_/  /'

		echo
		echo -e "${BLUE}Event Bus Metrics:${NC}"
		echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
		echo "$metrics_output" | grep -E '^haproxy_ic_event' | sed 's/haproxy_ic_/  /'

		echo
		echo -e "${GREEN}Duration Metrics (histograms):${NC}"
		echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
		echo "$metrics_output" | grep -E '^haproxy_ic_.*_duration_seconds_(sum|count)' | sed 's/haproxy_ic_/  /'

		echo
		ok "Use --raw to see full Prometheus output"
		ok "Use --watch to continuously monitor metrics"
		echo
		echo "Keep port-forward running:"
		echo "  kubectl -n $CTRL_NAMESPACE port-forward deploy/$ctrl_deploy 9090:9090"
		echo "  curl http://localhost:9090/metrics"
	fi
}

dev_up() {
    print_section "🏗️  Starting Development Environment"

    log INFO "Preflight checks..."
    require_cmd kind
    require_cmd kubectl
    require_cmd docker
    require_cmd helm
    ok "Dependencies present."

    ensure_cluster

    # Deploy blocklist-server BEFORE controller if echo demos are enabled
    # The controller's default template includes an HTTP Store demo that fetches
    # from blocklist-server.echo.svc - it must exist before controller starts
    if [[ "$SKIP_ECHO" != "true" ]]; then
        deploy_blocklist_server || {
            err "Blocklist server deployment failed"
            return 1
        }
        # auth-server fixture for the external-auth demo. The spoa-hub
        # external-auth plugin makes HTTP requests to auth-server.echo.svc
        # at runtime, so it must be reachable when the auth-allowed /
        # auth-denied ingresses are exercised.
        deploy_auth_server || {
            err "Auth server deployment failed"
            return 1
        }
    fi

    deploy_controller || {
        err "Controller deployment failed"
        return 1
    }

    deploy_haproxy || {
        err "HAProxy deployment failed"
        return 1
    }

    if [[ "$SKIP_ECHO" != "true" ]]; then
        deploy_echo_server
        deploy_auth_tls_fixtures
        deploy_ingress_demo
        deploy_gateway_demo
    fi

    post_deploy_tips
}


main() {
    parse_args "$@"
    compute_haproxy_image

    case "$COMMAND" in
        up)
            dev_up
            ;;
        down|clean)
            dev_down "$@"
            ;;
        logs)
            dev_logs
            ;;
        exec)
            dev_exec
            ;;
        restart)
            dev_restart
            ;;
        status)
            dev_status
            ;;
        metrics)
            dev_metrics "$@"
            ;;
        test)
            test_ingress
            ;;
        port-forward)
            port_forward_haproxy
            ;;
        *)
            err "Unknown command: $COMMAND"
            show_usage
            exit 1
            ;;
    esac
}

# Now that all functions are defined, set up the environment and trap
pushd "${REPO_ROOT}" >/dev/null 2>&1 || {
    echo "Error: Could not change to repository root directory"
    exit 1
}
trap cleanup_on_exit EXIT

main "$@"
