# CSI Secret Debugger

A Container Storage Interface (CSI) driver designed for debugging and testing secret storage workflows in Kubernetes.

## Overview

The CSI Secret Debugger is a specialized CSI driver that helps developers debug and test secret storage implementations. It provides visibility into how secrets are mounted, accessed, and managed within Kubernetes pods through CSI volumes.

## Features

- **Secret Mount Debugging**: Monitor and log how secrets are mounted into pods via CSI volumes
- **Storage Workflow Testing**: Validate secret persistence and lifecycle management
- **Flexible Configuration**: Support for various secret storage backends
- **Comprehensive Logging**: Detailed logs for troubleshooting secret mount issues

## Use Cases

- **Development & Testing**: Validate secret storage implementations before production deployment
- **Troubleshooting**: Diagnose issues with secret mounting in Kubernetes clusters
- **Education**: Understand how CSI drivers interact with secret storage systems
- **Integration Testing**: Verify secret storage behavior across different Kubernetes versions

## Quick Start

## Installation

```bash
# Deploy the CSI driver to your cluster
kubectl apply -f deploy/
```

### Building from Source

```bash
go build -o csi-debugger 
```

## Configuration

The driver can be configured through command-line flags:

- `--endpoint`: CSI socket endpoint (default: `unix:///csi/csi.sock`)
- `--nodeid`: Node ID for the driver instance
- `--driver-name`: CSI driver name (default: `csi-debugger-driver.csi.k8s.io`)

## E2E Testing

The project includes comprehensive end-to-end tests to validate secret storage workflows:

```sh
cd e2e
go test -v -tags e2e -timeout 20m

# to skip the teardown
SKIP_TEARDOWN=true go test -v -tags e2e -timeout 20m

# to reach the UI
kubectl port-forward -n kube-system svc/csi-driver-admin 8090:8090
```

### What the E2E Test Does

*   **StorageClass Creation**: Creates a StorageClass programmatically (Go struct) instead of reading a YAML file.
*   **VolumeBindingMode Handling**: Uses `WaitForFirstConsumer` to ensure the PV creates on the specific node where the Pod is scheduled, which is critical for many CSI drivers (like local storage or hostpath).
*   **Persistence Verification**: Performs a **Write -> Delete -> Recreate -> Read** cycle to prove that data was actually stored on the backend volume, not just in the Pod's ephemeral layer.
*   **Resource Cleanup**: Uses `defer` to clean up resources, ensuring your cluster is ready for the next test run even if this one fails.


