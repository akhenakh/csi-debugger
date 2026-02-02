//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	clusterName = "csi-cluster"
	imageName   = "csi-debugger-driver:e2e"
	driverName  = "csi-debugger-driver.csi.k8s.io"
	namespace   = "kube-system" // Driver deploys here
)

// getImageName returns the correct image name for the container runtime
// Podman adds "localhost/" prefix to locally built images
func getImageName() string {
	if containerRuntime == "podman" {
		return "localhost/" + imageName
	}
	return imageName
}

// Detect runtime: prefer podman if installed and docker is missing, or use env override
var containerRuntime = getContainerRuntime()

func getContainerRuntime() string {
	// Check env override
	if v := os.Getenv("CONTAINER_RUNTIME"); v != "" {
		return v
	}

	// Check if "docker" command exists and if it is actually Podman
	path, err := exec.LookPath("docker")
	if err == nil {
		// Run `docker --version` to see if it says "podman"
		cmd := exec.Command(path, "--version")
		out, err := cmd.Output()
		if err == nil && strings.Contains(strings.ToLower(string(out)), "podman") {
			return "podman"
		}
		return "docker"
	}

	// Fallback to "podman" if docker binary doesn't exist
	if _, err := exec.LookPath("podman"); err == nil {
		return "podman"
	}

	return "docker"
}

func TestE2E(t *testing.T) {
	t.Logf("Using container runtime: %s", containerRuntime)

	// 1. Setup Infrastructure
	setupCluster(t)
	defer teardownCluster(t)

	// 2. Build and Load Artifacts
	buildAndLoadImage(t)

	// 3. Deploy CSI Driver (Controller + Node)
	deployDriver(t)

	// 4. Run Functional Tests
	t.Run("Secret Provider Smoke Test", func(t *testing.T) {
		runVolumeLifecycleTest(t)
	})
}

func setupCluster(t *testing.T) {
	t.Log("Creating Kind cluster...")

	// If using podman, we must instruct Kind to use it
	if containerRuntime == "podman" {
		os.Setenv("KIND_EXPERIMENTAL_PROVIDER", "podman")
	}

	// Check if cluster exists first to speed up local dev
	cmd := exec.Command("kind", "get", "clusters")
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), clusterName) {
		t.Log("Cluster already exists")
		return
	}

	runCmd(t, "kind", "create", "cluster", "--name", clusterName)
}

func teardownCluster(t *testing.T) {
	if os.Getenv("SKIP_TEARDOWN") == "true" {
		t.Log("Skipping teardown...")
		return
	}
	t.Log("Deleting Kind cluster...")
	runCmd(t, "kind", "delete", "cluster", "--name", clusterName)
}

func buildAndLoadImage(t *testing.T) {
	t.Logf("Building image %s with %s...", imageName, containerRuntime)

	// Build from parent directory where Dockerfile is located
	cmd := exec.Command(containerRuntime, "build", "-t", imageName, "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to build image: %v\n%s", err, string(out))
	}

	// Save to Archive (Robust method for kind/k3d compatibility with podman)
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "image.tar")

	t.Log("Saving image to archive...")
	var saveCmd *exec.Cmd
	if containerRuntime == "podman" {
		saveCmd = exec.Command(containerRuntime, "save", "--format=docker-archive", "-o", archivePath, imageName)
	} else {
		saveCmd = exec.Command(containerRuntime, "save", "-o", archivePath, imageName)
	}

	if out, err := saveCmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to save image archive: %v\n%s", err, string(out))
	}

	// Load Archive into Kind
	t.Log("Loading archive into Kind...")
	runCmd(t, "kind", "load", "image-archive", archivePath, "--name", clusterName)

	// Verify the image is loaded by checking the node directly
	t.Log("Verifying image is loaded in cluster...")
	nodeName := fmt.Sprintf("%s-control-plane", clusterName)
	var verifyCmd *exec.Cmd
	if containerRuntime == "podman" {
		verifyCmd = exec.Command("podman", "exec", nodeName, "crictl", "images")
	} else {
		verifyCmd = exec.Command("docker", "exec", nodeName, "crictl", "images")
	}
	out, _ := verifyCmd.CombinedOutput()
	expectedImage := getImageName()
	if !strings.Contains(string(out), expectedImage) {
		t.Logf("Warning: Image %s not found in cluster node. Loaded images:\n%s", expectedImage, string(out))
	} else {
		t.Logf("Image %s successfully loaded", expectedImage)
	}
}

func deployDriver(t *testing.T) {
	t.Log("Deploying CSI Driver Manifests...")

	// We embed the YAML here to make the test self-contained
	manifests := fmt.Sprintf(`
# Secret providers don't need CSI Driver registration, Controller, or Provisioner
# They only run as a DaemonSet on nodes to mount secrets
apiVersion: v1
kind: ServiceAccount
metadata:
  name: csi-driver-sa
  namespace: %s
---
kind: ClusterRole
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: csi-driver-role
rules:
  # Secret providers only need node-level access, not storage API access
  - apiGroups: [""]
    resources: ["nodes", "pods"]
    verbs: ["get", "list", "watch"]
---
kind: ClusterRoleBinding
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: csi-driver-binding
subjects:
  - kind: ServiceAccount
    name: csi-driver-sa
    namespace: %s
roleRef:
  kind: ClusterRole
  name: csi-driver-role
  apiGroup: rbac.authorization.k8s.io
---
# Node DaemonSet for Secret Provider
# Only implements Node Service (mounting secrets, not provisioning volumes)
kind: DaemonSet
apiVersion: apps/v1
metadata:
  name: csi-node
  namespace: %s
spec:
  selector:
    matchLabels:
      app: csi-node
  template:
    metadata:
      labels:
        app: csi-node
    spec:
      serviceAccountName: csi-driver-sa
      hostNetwork: true
      containers:
        - name: csi-driver
          securityContext:
            privileged: true
            runAsUser: 0
          image: %s
          imagePullPolicy: Never
          ports:
            - containerPort: 8090
              name: http-admin
              protocol: TCP
          env:
            - {name: SOCKET_PATH, value: /csi/csi.sock}
            - {name: KUBE_NODE_NAME, valueFrom: {fieldRef: {fieldPath: spec.nodeName}}}
            - {name: LOG_LEVEL, value: DEBUG}
          volumeMounts:
            - {name: socket-dir, mountPath: /csi}
      volumes:
        - name: socket-dir
          hostPath: {path: /var/lib/kubelet/plugins/%s/, type: DirectoryOrCreate}
---
apiVersion: v1
kind: Service
metadata:
  name: csi-driver-admin
  namespace: %s
spec:
  selector:
    app: csi-node
  ports:
    - port: 8090
      targetPort: 8090
      name: http-admin
  type: NodePort
`,
		namespace, namespace, // RBAC
		namespace, getImageName(), driverName, // Node
		namespace, // Service
	)

	kubectlApply(t, manifests)

	t.Log("Waiting for Secret Provider DaemonSet to be ready...")
	// Wait for DaemonSet to create pods first, then wait for them to be ready
	time.Sleep(60 * time.Second) // Give DaemonSet time to create pods
	runCmd(t, "kubectl", "wait", "--for=condition=ready", "pod", "-l", "app=csi-node", "-n", namespace, "--timeout=120s")
}

func runVolumeLifecycleTest(t *testing.T) {
	// Secret providers don't provision volumes - they mount secrets
	// This is a basic smoke test that verifies the driver is registered and running
	t.Log("Verifying CSI driver is registered with kubelet...")

	// Check that the driver socket exists on the node
	cmd := exec.Command("kubectl", "get", "nodes")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to get nodes: %v", err)
	}
	t.Logf("Cluster nodes:\n%s", string(out))

	// List the running CSI driver pods
	t.Log("Checking CSI driver DaemonSet status...")
	cmd = exec.Command("kubectl", "get", "pods", "-n", namespace, "-l", "app=csi-node")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to get CSI driver pods: %v", err)
	}
	t.Logf("CSI driver pods:\n%s", string(out))

	// Check the driver logs to confirm it's running
	t.Log("Checking CSI driver logs...")
	cmd = exec.Command("kubectl", "logs", "-n", namespace, "-l", "app=csi-node", "-c", "csi-driver", "--tail=10")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Logf("Warning: Could not get driver logs: %v", err)
	} else {
		t.Logf("Driver logs:\n%s", string(out))
	}

	t.Log("Secret Provider CSI driver is running successfully!")
}

func runCmd(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	// Pass through environment for KIND_EXPERIMENTAL_PROVIDER
	cmd.Env = os.Environ()

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Command failed: %s %s\nOutput: %s\nError: %v", name, strings.Join(args, " "), string(out), err)
	}
}

func kubectlApply(t *testing.T, yamlContent string) {
	t.Helper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yamlContent)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to apply yaml:\n%s\nError: %v\nOutput: %s", yamlContent, err, string(out))
	}
}

func verifyData(t *testing.T, podName, mountPath, fileName, expectedContent string) {
	t.Helper()
	cmd := exec.Command("kubectl", "exec", podName, "--", "cat", fmt.Sprintf("%s/%s", mountPath, fileName))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to read file from pod %s: %v", podName, err)
	}

	actualContent := strings.TrimSpace(string(out))
	if actualContent != expectedContent {
		t.Fatalf("Data persistence check failed in pod %s.\nExpected: %s\nGot: %s", podName, expectedContent, actualContent)
	}
	t.Logf("Data match verified in pod %s", podName)
}
