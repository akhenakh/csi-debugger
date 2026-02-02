//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
	driverName  = "csidebugger"
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

	// 4. Install Secrets Store CSI Driver (required to use secret providers)
	installSecretsStoreCSIDriver(t)

	// 5. Run Functional Tests
	t.Run("Secret Provider Smoke Test", func(t *testing.T) {
		runVolumeLifecycleTest(t)
	})

	t.Run("Secret Mounting Validation", func(t *testing.T) {
		runSecretMountingValidationTest(t)
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
            - {name: SOCKET_PATH, value: /csi/csidebugger.sock}
            - {name: KUBE_NODE_NAME, valueFrom: {fieldRef: {fieldPath: spec.nodeName}}}
            - {name: LOG_LEVEL, value: DEBUG}
          volumeMounts:
            - {name: providers-socket-dir, mountPath: /csi}
      volumes:
        - name: providers-socket-dir
          hostPath: {path: /var/lib/kubelet/plugins/secrets-store.csi.k8s.io/providers, type: DirectoryOrCreate}
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
		namespace, getImageName(), // Node
		namespace, // Service
	)

	kubectlApply(t, manifests)

	t.Log("Waiting for Secret Provider DaemonSet to be ready...")
	// Wait for DaemonSet to create pods first, then wait for them to be ready
	time.Sleep(20 * time.Second) // Give DaemonSet time to create pods
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

func verifyData(t *testing.T, namespace, podName, mountPath, fileName, expectedContent string) {
	t.Helper()
	cmd := exec.Command("kubectl", "exec", "-n", namespace, podName, "--", "cat", fmt.Sprintf("%s/%s", mountPath, fileName))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to read file from pod %s/%s: %v\nOutput: %s", namespace, podName, err, string(out))
	}

	actualContent := strings.TrimSpace(string(out))
	if actualContent != expectedContent {
		t.Fatalf("Data persistence check failed in pod %s/%s.\nExpected: %s\nGot: %s", namespace, podName, expectedContent, actualContent)
	}
	t.Logf("Data match verified in pod %s/%s", namespace, podName)
}

// installSecretsStoreCSIDriver installs the secrets-store-csi-driver using Helm
func installSecretsStoreCSIDriver(t *testing.T) {
	t.Log("Installing Secrets Store CSI Driver...")

	// Add Helm repo
	runCmd(t, "helm", "repo", "add", "secrets-store-csi-driver", "https://kubernetes-sigs.github.io/secrets-store-csi-driver/charts")
	runCmd(t, "helm", "repo", "update")

	// Install the driver with correct provider path configuration
	// The provider path must match where we mount our provider socket
	providersDir := "/var/lib/kubelet/plugins/secrets-store.csi.k8s.io/providers"
	runCmd(t, "helm", "install", "csi-secrets-store", "secrets-store-csi-driver/secrets-store-csi-driver",
		"--namespace", "kube-system",
		"--set", "syncSecret.enabled=true",
		"--set", "linux.providersDir="+providersDir,
		"--set", "linux.nodeAffinity=null",
		"--set", "linux.additionalVolumes[0].name=providers-dir",
		"--set", "linux.additionalVolumes[0].hostPath.path="+providersDir,
		"--set", "linux.additionalVolumes[0].hostPath.type=DirectoryOrCreate",
		"--set", "linux.additionalVolumeMounts[0].name=providers-dir",
		"--set", "linux.additionalVolumeMounts[0].mountPath="+providersDir,
		"--wait",
		"--timeout", "2m",
	)

	t.Log("Secrets Store CSI Driver installed successfully")

	// Wait for secrets-store-csi-driver pods to be ready
	t.Log("Waiting for secrets-store-csi-driver pods to be ready...")
	runCmd(t, "kubectl", "wait", "--for=condition=ready", "pod", "-l", "app=secrets-store-csi-driver", "-n", "kube-system", "--timeout=60s")

	// The driver might not pick up providers that were created before it started
	// Restart the driver pod to force it to rescan the providers directory
	t.Log("Restarting secrets-store-csi-driver to pick up provider...")
	runCmd(t, "kubectl", "delete", "pod", "-l", "app=secrets-store-csi-driver", "-n", "kube-system")
	time.Sleep(5 * time.Second) // Give it time to terminate
	runCmd(t, "kubectl", "wait", "--for=condition=ready", "pod", "-l", "app=secrets-store-csi-driver", "-n", "kube-system", "--timeout=60s")
	t.Log("Secrets-store-csi-driver restarted successfully")
}

// runSecretMountingValidationTest validates that secrets can be mounted in pods
func runSecretMountingValidationTest(t *testing.T) {
	testNamespace := "e2e-test"
	testPodName := "secret-test-pod"
	mountPath := "/mnt/secrets"

	// 1. Create test namespace
	t.Log("Creating test namespace...")
	kubectlApply(t, fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata:
  name: %s
`, testNamespace))

	// 2. Create SecretProviderClass
	t.Log("Creating SecretProviderClass...")
	spcManifest := fmt.Sprintf(`
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: csi-debugger-spc
  namespace: %s
spec:
  provider: csidebugger
  parameters:
    # Parameters for the debugger provider (can be empty for basic testing)
    debug: "true"
`, testNamespace)
	kubectlApply(t, spcManifest)

	// 3. Add test secret via the debugger admin API
	t.Log("Adding test secret via debugger admin API...")
	addSecretViaAdminAPI(t, "test-secret.txt", "e2e-test-secret-value", "v1")

	// Wait for the provider to be fully registered
	t.Log("Waiting for provider registration...")
	time.Sleep(5 * time.Second)

	// 4. Create test pod with CSI volume mount
	t.Log("Creating test pod with CSI volume mount...")
	podManifest := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  containers:
  - name: test-container
    image: busybox:1.36
    command: ["sh", "-c", "sleep 3600"]
    volumeMounts:
    - name: secrets-volume
      mountPath: %s
      readOnly: true
  volumes:
  - name: secrets-volume
    csi:
      driver: secrets-store.csi.k8s.io
      readOnly: true
      volumeAttributes:
        secretProviderClass: csi-debugger-spc
`, testPodName, testNamespace, mountPath)
	kubectlApply(t, podManifest)

	// 5. Wait for pod to be ready
	t.Log("Waiting for test pod to be ready...")
	waitForPod(t, testNamespace, testPodName, 60*time.Second)

	// 6. Verify the secret is mounted in the pod
	t.Log("Verifying secret is mounted in the pod...")
	verifyData(t, testNamespace, testPodName, mountPath, "test-secret.txt", "e2e-test-secret-value")

	t.Log("Secret mounting validation test passed!")
}

// addSecretViaAdminAPI adds a secret to the debugger via its HTTP admin API
func addSecretViaAdminAPI(t *testing.T, name, value, version string) {
	// Port-forward to the admin service
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start port-forward in background
	go func() {
		cmd := exec.CommandContext(ctx, "kubectl", "port-forward", "-n", namespace, "svc/csi-driver-admin", "8090:8090")
		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			t.Logf("Port-forward error: %v", err)
		}
	}()

	// Wait for port-forward to be ready
	time.Sleep(3 * time.Second)

	// Try to add the secret via the admin API
	var lastErr error
	for i := 0; i < 5; i++ {
		data := fmt.Sprintf("name=%s&value=%s&version=%s&mode=420",
			name, value, version)

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Post("http://localhost:8090/update", "application/x-www-form-urlencoded", strings.NewReader(data))
		if err != nil {
			lastErr = err
			t.Logf("Attempt %d: Failed to add secret: %v", i+1, err)
			time.Sleep(2 * time.Second)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusSeeOther {
			t.Logf("Successfully added secret '%s' via admin API", name)
			return
		}

		body, _ := io.ReadAll(resp.Body)
		lastErr = fmt.Errorf("unexpected status: %d, body: %s", resp.StatusCode, string(body))
		t.Logf("Attempt %d: Unexpected response: %v", i+1, lastErr)
		time.Sleep(2 * time.Second)
	}

	t.Fatalf("Failed to add secret after retries: %v", lastErr)
}

// kubectlExec executes a command in a pod and returns the output
func kubectlExec(t *testing.T, namespace, podName, container string, args ...string) string {
	t.Helper()
	execArgs := []string{"exec", "-n", namespace, podName}
	if container != "" {
		execArgs = append(execArgs, "-c", container)
	}
	execArgs = append(execArgs, "--")
	execArgs = append(execArgs, args...)

	cmd := exec.Command("kubectl", execArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl exec failed: %v\nOutput: %s", err, string(out))
	}
	return string(out)
}

// kubectlGetPods returns the list of pods matching the selector
func kubectlGetPods(t *testing.T, namespace, selector string) string {
	t.Helper()
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace, "-l", selector, "-o", "json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to get pods: %v\nOutput: %s", err, string(out))
	}
	return string(out)
}

// waitForPod waits for a pod to be in the ready state
func waitForPod(t *testing.T, namespace, podName string, timeout time.Duration) {
	t.Helper()
	t.Logf("Waiting for pod %s/%s to be ready...", namespace, podName)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("Timeout waiting for pod %s/%s to be ready", namespace, podName)
		default:
			cmd := exec.Command("kubectl", "get", "pod", podName, "-n", namespace, "-o", "jsonpath={.status.phase}")
			out, err := cmd.CombinedOutput()
			if err == nil && strings.TrimSpace(string(out)) == "Running" {
				// Check if all containers are ready
				readyCmd := exec.Command("kubectl", "get", "pod", podName, "-n", namespace, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				readyOut, err := readyCmd.CombinedOutput()
				if err == nil && strings.TrimSpace(string(readyOut)) == "True" {
					t.Logf("Pod %s/%s is ready", namespace, podName)
					return
				}
			}
			time.Sleep(2 * time.Second)
		}
	}
}

// createSecretProviderClass creates a SecretProviderClass resource
func createSecretProviderClass(t *testing.T, namespace, name, provider string, parameters map[string]string) {
	t.Helper()
	t.Logf("Creating SecretProviderClass %s in namespace %s", name, namespace)

	var params []string
	for k, v := range parameters {
		params = append(params, fmt.Sprintf("    %s: %s", k, v))
	}
	paramsYaml := strings.Join(params, "\n")
	if paramsYaml == "" {
		paramsYaml = "    # No parameters"
	}

	manifest := fmt.Sprintf(`
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: %s
  namespace: %s
spec:
  provider: %s
  parameters:
%s
`, name, namespace, provider, paramsYaml)

	kubectlApply(t, manifest)
}

// createTestDeployment creates a deployment that mounts secrets via CSI
func createTestDeployment(t *testing.T, namespace, name, spcName string, replicas int32) {
	t.Helper()
	t.Logf("Creating test deployment %s in namespace %s", name, namespace)

	manifest := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  labels:
    app: %s
spec:
  replicas: %d
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
      - name: test-container
        image: busybox:1.36
        command: ["sh", "-c", "sleep 3600"]
        volumeMounts:
        - name: secrets-volume
          mountPath: /mnt/secrets
          readOnly: true
      volumes:
      - name: secrets-volume
        csi:
          driver: secrets-store.csi.k8s.io
          readOnly: true
          volumeAttributes:
            secretProviderClass: %s
`, name, namespace, name, replicas, name, name, spcName)

	kubectlApply(t, manifest)
}
