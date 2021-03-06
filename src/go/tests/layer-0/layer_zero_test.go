// layer_0 contains a test that brings up layer 0 (a single-node Kubernetes
// cluster) on a GCP VM. To run it on your workstation, first create a service
// account:
//   https://console.cloud.google.com/apis/credentials/serviceaccountkey
// make it a "Compute Admin", and download the JSON key. Go to:
//   https://console.cloud.google.com/iam-admin/serviceaccounts
// tick the box by ######-compute@, and make your new SA a "Service Account
// User". You might need to wait ~10 minutes for IAM to propagate. Then replace
// your-project-id below and run:
//   bazel test \
//     --test_env GCP_PROJECT_ID=your-project-id --test_env GCP_ZONE=europe-west1-c \
//     --test_env GOOGLE_APPLICATION_CREDENTIALS=path/to/credentials.json \
//     --test_tag_filters=external //src/go/tests/layer-0:all

package layer_0

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	mrand "math/rand"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apcera/libretto/ssh"
	"github.com/apcera/libretto/virtualmachine/gcp"
	cssh "golang.org/x/crypto/ssh"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"
)

const (
	alphanum = "abcdefghijklmnopqrstuvwxyz0123456789"

	// Note: to use a workspace-relative path, the go_test() must have `rundir = "."`.
	clusterInstallScript = "./src/bootstrap/robot/install_k8s_on_robot.sh"
)

var (
	GCP_PROJECT_ID                 = mustGetenv("GCP_PROJECT_ID")
	GCP_ZONE                       = mustGetenv("GCP_ZONE")
	GOOGLE_APPLICATION_CREDENTIALS = mustGetenv("GOOGLE_APPLICATION_CREDENTIALS")
)

func init() {
	mrand.Seed(time.Now().UnixNano())
}

func mustGetenv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		log.Fatalf("ERROR: environment variable %s must be non-empty", key)
	}
	return value
}

func randomString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphanum[mrand.Intn(len(alphanum))]
	}
	return string(b)
}

// generateKeypair generates an SSH keypair that will be used to connect to the VM.
func generateKeypair() ([]byte, []byte, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate SSH private key: %v", err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: nil,
		Bytes:   x509.MarshalPKCS1PrivateKey(privateKey),
	})
	publicKey, err := cssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate SSH public key: %v", err)
	}
	publicPEM := cssh.MarshalAuthorizedKey(publicKey)
	return privatePEM, publicPEM, nil
}

// getImageName gets the name of the latest image in a given family.
func getImageName(project, family string) (string, error) {
	// Use oauth2.NoContext if there isn't a good context to pass in.
	ctx := context.Background()

	client, err := google.DefaultClient(ctx, compute.ComputeScope)
	if err != nil {
		return "", err
	}
	computeService, err := compute.New(client)
	if err != nil {
		return "", err
	}
	image, err := computeService.Images.GetFromFamily(project, family).Do()
	return image.Name, err
}

func newVM(privatePEM []byte, publicPEM []byte, imageProject, imageFamily string) (*gcp.VM, error) {
	sourceImage, err := getImageName(imageProject, imageFamily)
	if err != nil {
		return nil, err
	}

	return &gcp.VM{
		Name:          "layer-zero-test-vm-" + randomString(8),
		Zone:          GCP_ZONE,
		MachineType:   "n1-standard-4",
		SourceImage:   sourceImage,
		ImageProjects: []string{imageProject},
		Disks: []gcp.Disk{
			{
				DiskType:   "pd-standard",
				DiskSizeGb: 200,
				AutoDelete: true,
			},
		},
		Network:     "default",
		Subnetwork:  "default",
		Project:     GCP_PROJECT_ID,
		Scopes:      []string{"https://www.googleapis.com/auth/cloud-platform"},
		AccountFile: GOOGLE_APPLICATION_CREDENTIALS,
		SSHCreds: ssh.Credentials{
			SSHUser:       "robot",
			SSHPrivateKey: string(privatePEM),
		},
		SSHPublicKey: string(publicPEM),
		Tags:         []string{"delete-after-one-day"},
	}, nil
}

// isStartedByKokoro checks whether the test was started by Kokoro by checking
// the hostname. This is used to identify whether the VM's private IP should be
// used for SSH connections (valid from within the same cloud project as the
// target VM), or whether to use the public IP (valid from a dev workstation).
func isStartedByKokoro() (bool, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return false, fmt.Errorf("failed to get hostname: %v", err)
	}
	return strings.HasPrefix(hostname, "kokoro"), nil
}

// getSSH gets a connected SSH client, connecting to the public or private IP
// depending on how the test was started.
func getSSH(t *testing.T, vm *gcp.VM) (ssh.Client, error) {
	ips, err := vm.GetIPs()
	if err != nil {
		return nil, err
	}

	client := &ssh.SSHClient{
		Creds: &vm.SSHCreds,
		Port:  22,
	}

	if onKokoro, err := isStartedByKokoro(); err != nil {
		return nil, err
	} else if onKokoro {
		client.IP = ips[gcp.PrivateIP]
		t.Logf("Detected that VM is started by Kokoro, so using private IP: %v", client.IP)
	} else {
		client.IP = ips[gcp.PublicIP]
		t.Logf("Using public VM IP: %v", client.IP)
	}

	if err := client.WaitForSSH(5 * time.Minute); err != nil {
		return nil, err
	}

	return client, nil
}

// uploadLocalFile uploads a local file over SSH.
func uploadLocalFile(client ssh.Client, localPath string, remotePath string, mode uint32) error {
	s, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("failed to stat %s: %v", localPath, err)
	}
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open %s: %v", localPath, err)
	}
	if err := client.Upload(f, remotePath, int(s.Size()), mode); err != nil {
		return fmt.Errorf("failed to upload to %s: %v", remotePath, err)
	}
	return nil
}

// testCommand runs a command that is expected to succeed. If it fails, it
// logs the command's output and returns an error.
func testCommand(t *testing.T, client ssh.Client, command string) error {
	// To avoid mixing the output streams of multiple commands running in
	// parallel tests, we combine stdout and stderr on the remote host, and
	// capture stdout and write it to the test log here.
	stdout := new(bytes.Buffer)
	if err := client.Run(command+" 2>&1", stdout, os.Stderr); err != nil {
		t.Logf("`%s` failed with %v:\n%s", command, err, stdout.String())
		return err
	}
	return nil
}

// logCommand runs a command that could be helpful for debugging. The command's
// output is written to the test log.
func logCommand(t *testing.T, client ssh.Client, command string) {
	stdout := new(bytes.Buffer)
	err := client.Run(command+" 2>&1", stdout, os.Stderr)
	t.Logf("+ %s\n%s", command, stdout.String())
	if err != nil {
		t.Logf("`%s` failed with %v:\n", command, err)
	}
}

// uploadAndRun uploads a local bash script over SSH and runs it.
func uploadAndRun(t *testing.T, client ssh.Client, localPath string, args ...string) error {
	remotePath := "~/" + path.Base(localPath)
	if err := uploadLocalFile(client, localPath, remotePath, 0755); err != nil {
		return err
	}
	quotedArgs := make([]string, len(args))
	for i, arg := range args {
		quotedArgs[i] = strconv.Quote(arg)
	}
	command := fmt.Sprintf(`bash -x %s %s`, remotePath, strings.Join(quotedArgs, " "))
	if err := testCommand(t, client, command); err != nil {
		return fmt.Errorf("failed to run `%s` on VM: %v", remotePath, err)
	}
	return nil
}

// testClusterNetworking checks that the internet is reachable from the cluster.
// It does this with a K8s Job that uses busybox to ping google.com.
func testClusterNetworking(t *testing.T, client ssh.Client) error {
	remotePath := "~/job.yaml"
	jobFile := bytes.NewBufferString(`
apiVersion: batch/v1
kind: Job
metadata:
  name: ping
spec:
  template:
    spec:
      containers:
      - name: ping
        image: busybox
        command: ["ping",  "-c1", "google.com"]
      restartPolicy: OnFailure
`)

	if err := client.Upload(jobFile, remotePath, jobFile.Len(), 0644); err != nil {
		return fmt.Errorf("Failed to upload to %s: %v", remotePath, err)
	}
	if err := testCommand(t, client, "kubectl create -f "+remotePath); err != nil {
		return fmt.Errorf("failed to create job on VM: %v", err)
	}
	waitCommand := "kubectl wait --for condition=Complete job/ping --timeout=5m"
	if err := testCommand(t, client, waitCommand); err != nil {
		t.Error("Job `ping` failed to complete on VM")
		// Dump pod logs to help debug failure.
		logCommand(t, client, "kubectl logs -l job-name=ping")
	}
	return nil
}

// TestInstallLayerZero creates a new VM and installs a Kubernetes cluster on it.
func TestInstallLayerZero(t *testing.T) {
	privatePEM, publicPEM, err := generateKeypair()
	if err != nil {
		t.Fatal(err)
	}

	// Together, these identify the public image used for the VM, without
	// pinning a specific version. We test each image in parallel as the tests
	// are independent and take 3-4 minutes.
	testCases := []struct {
		imageProject string
		imageFamily  string
	}{
		{"ubuntu-os-cloud", "ubuntu-1404-lts"},
		{"ubuntu-os-cloud", "ubuntu-1604-lts"},
		{"ubuntu-os-cloud", "ubuntu-1804-lts"},
	}
	for _, tc := range testCases {
		vm, err := newVM(privatePEM, publicPEM, tc.imageProject, tc.imageFamily)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(tc.imageFamily, func(t *testing.T) {
			t.Parallel()
			defer func() {
				t.Log("Deleting VM...")
				if err := vm.Destroy(); err != nil {
					t.Error("Failed to delete VM:", err)
				}
			}()

			t.Logf("Provisioning VM %s...", vm.Name)
			if err := vm.Provision(); err != nil {
				t.Fatal("Failed to provision VM:", err)
			}

			t.Log("Establishing SSH connection to VM...")
			client, err := getSSH(t, vm)
			if err != nil {
				t.Fatal("Failed to SSH to VM:", err)
			}

			t.Log("Installing cluster on VM...")
			if err := uploadAndRun(t, client, clusterInstallScript); err != nil {
				t.Fatal("Failed to install cluster:", err)
			}

			t.Log("Testing networking on VM cluster...")
			if err := testClusterNetworking(t, client); err != nil {
				t.Fatal("Failed networking test:", err)
			}
		})
	}
}
