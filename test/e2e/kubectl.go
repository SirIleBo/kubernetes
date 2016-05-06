/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/ghodss/yaml"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/annotations"
	apierrs "k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/unversioned"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/registry/generic/registry"
	pkgutil "k8s.io/kubernetes/pkg/util"
	utilnet "k8s.io/kubernetes/pkg/util/net"
	"k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/pkg/version"
	"k8s.io/kubernetes/test/e2e/framework"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	nautilusImage            = "gcr.io/google_containers/update-demo:nautilus"
	kittenImage              = "gcr.io/google_containers/update-demo:kitten"
	updateDemoSelector       = "name=update-demo"
	updateDemoContainer      = "update-demo"
	frontendSelector         = "app=guestbook,tier=frontend"
	redisMasterSelector      = "app=redis,role=master"
	redisSlaveSelector       = "app=redis,role=slave"
	goproxyContainer         = "goproxy"
	goproxyPodSelector       = "name=goproxy"
	netexecContainer         = "netexec"
	netexecPodSelector       = "name=netexec"
	kubectlProxyPort         = 8011
	guestbookStartupTimeout  = 10 * time.Minute
	guestbookResponseTimeout = 3 * time.Minute
	simplePodSelector        = "name=nginx"
	simplePodName            = "nginx"
	nginxDefaultOutput       = "Welcome to nginx!"
	simplePodPort            = 80
	pausePodSelector         = "name=pause"
	pausePodName             = "pause"
	runJobTimeout            = 5 * time.Minute
	busyboxImage             = "gcr.io/google_containers/busybox:1.24"
	nginxImage               = "gcr.io/google_containers/nginx:1.7.9"
	kubeCtlManifestPath      = "test/e2e/testing-manifests/kubectl"
	redisControllerFilename  = "redis-master-controller.json"
	redisServiceFilename     = "redis-master-service.json"
)

var (
	proxyRegexp = regexp.MustCompile("Starting to serve on 127.0.0.1:([0-9]+)")

	// Extended pod logging options were introduced in #13780 (v1.1.0) so we don't expect tests
	// that rely on extended pod logging options to work on clusters before that.
	//
	// TODO(ihmccreery): remove once we don't care about v1.0 anymore, (tentatively in v1.3).
	extendedPodLogFilterVersion = version.MustParse("v1.1.0")

	// NodePorts were made optional in #12831 (v1.1.0) so we don't expect tests that used to
	// require NodePorts but no longer include them to work on clusters before that.
	//
	// TODO(ihmccreery): remove once we don't care about v1.0 anymore, (tentatively in v1.3).
	nodePortsOptionalVersion = version.MustParse("v1.1.0")

	// Jobs were introduced in v1.1, so we don't expect tests that rely on jobs to work on
	// clusters before that.
	//
	// TODO(ihmccreery): remove once we don't care about v1.0 anymore, (tentatively in v1.3).
	jobsVersion = version.MustParse("v1.1.0")

	// Deployments were introduced by default in v1.2, so we don't expect tests that rely on
	// deployments to work on clusters before that.
	//
	// TODO(ihmccreery): remove once we don't care about v1.1 anymore, (tentatively in v1.4).
	deploymentsVersion = version.MustParse("v1.2.0-alpha.7.726")

	// Pod probe parameters were introduced in #15967 (v1.2) so we dont expect tests that use
	// these probe parameters to work on clusters before that.
	//
	// TODO(ihmccreery): remove once we don't care about v1.1 anymore, (tentatively in v1.4).
	podProbeParametersVersion = version.MustParse("v1.2.0-alpha.4")
)

// Stops everything from filePath from namespace ns and checks if everything matching selectors from the given namespace is correctly stopped.
// Aware of the kubectl example files map.
func cleanupKubectlInputs(fileContents string, ns string, selectors ...string) {
	By("using delete to clean up resources")
	var nsArg string
	if ns != "" {
		nsArg = fmt.Sprintf("--namespace=%s", ns)
	}
	// support backward compatibility : file paths or raw json - since we are removing file path
	// dependencies from this test.
	framework.RunKubectlOrDieInput(fileContents, "delete", "--grace-period=0", "-f", "-", nsArg)
	framework.AssertCleanup(ns, selectors...)
}

func readTestFileOrDie(file string) []byte {
	return framework.ReadOrDie(path.Join(kubeCtlManifestPath, file))
}

func runKubectlRetryOrDie(args ...string) string {
	var err error
	var output string
	for i := 0; i < 3; i++ {
		output, err = framework.RunKubectl(args...)
		if err == nil || !strings.Contains(err.Error(), registry.OptimisticLockErrorMsg) {
			break
		}
		time.Sleep(time.Second)
	}
	// Expect no errors to be present after retries are finished
	// Copied from framework #ExecOrDie
	framework.Logf("stdout: %q", output)
	Expect(err).NotTo(HaveOccurred())
	return output
}

var _ = framework.KubeDescribe("Kubectl client", func() {
	defer GinkgoRecover()
	f := framework.NewDefaultFramework("kubectl")

	// Reustable cluster state function.  This won't be adversly affected by lazy initialization of framework.
	clusterState := func() *framework.ClusterVerification {
		return f.NewClusterVerification(
			framework.PodStateVerification{
				Selectors:   map[string]string{"app": "redis"},
				ValidPhases: []api.PodPhase{api.PodRunning /*api.PodPending*/},
			})
	}

	forEachPod := func(podFunc func(p api.Pod)) {
		clusterState().ForEach(podFunc)
	}
	var c *client.Client
	var ns string
	BeforeEach(func() {
		c = f.Client
		ns = f.Namespace.Name
	})

	// Customized Wait  / ForEach wrapper for this test.  These demonstrate the
	// idiomatic way to wrap the ClusterVerification structs for syntactic sugar in large
	// test files.
	// Print debug info if atLeast Pods are not found before the timeout
	waitForOrFailWithDebug := func(atLeast int) {
		pods, err := clusterState().WaitFor(atLeast, framework.PodStartTimeout)
		if err != nil || len(pods) < atLeast {
			// TODO: Generalize integrating debug info into these tests so we always get debug info when we need it
			framework.DumpAllNamespaceInfo(c, ns)
			framework.Failf("Verified %v of %v pods , error : %v", len(pods), atLeast, err)
		}
	}

	framework.KubeDescribe("Update Demo", func() {
		var nautilus, kitten []byte
		BeforeEach(func() {
			updateDemoRoot := "docs/user-guide/update-demo"
			nautilus = framework.ReadOrDie(filepath.Join(updateDemoRoot, "nautilus-rc.yaml"))
			kitten = framework.ReadOrDie(filepath.Join(updateDemoRoot, "kitten-rc.yaml"))
		})
		It("should create and stop a replication controller [Conformance]", func() {
			defer cleanupKubectlInputs(string(nautilus), ns, updateDemoSelector)

			By("creating a replication controller")
			framework.RunKubectlOrDieInput(string(nautilus[:]), "create", "-f", "-", fmt.Sprintf("--namespace=%v", ns))
			framework.ValidateController(c, nautilusImage, 2, "update-demo", updateDemoSelector, getUDData("nautilus.jpg", ns), ns)
		})

		It("should scale a replication controller [Conformance]", func() {
			defer cleanupKubectlInputs(string(nautilus[:]), ns, updateDemoSelector)

			By("creating a replication controller")
			framework.RunKubectlOrDieInput(string(nautilus[:]), "create", "-f", "-", fmt.Sprintf("--namespace=%v", ns))
			framework.ValidateController(c, nautilusImage, 2, "update-demo", updateDemoSelector, getUDData("nautilus.jpg", ns), ns)
			By("scaling down the replication controller")
			framework.RunKubectlOrDie("scale", "rc", "update-demo-nautilus", "--replicas=1", "--timeout=5m", fmt.Sprintf("--namespace=%v", ns))
			framework.ValidateController(c, nautilusImage, 1, "update-demo", updateDemoSelector, getUDData("nautilus.jpg", ns), ns)
			By("scaling up the replication controller")
			framework.RunKubectlOrDie("scale", "rc", "update-demo-nautilus", "--replicas=2", "--timeout=5m", fmt.Sprintf("--namespace=%v", ns))
			framework.ValidateController(c, nautilusImage, 2, "update-demo", updateDemoSelector, getUDData("nautilus.jpg", ns), ns)
		})

		It("should do a rolling update of a replication controller [Conformance]", func() {
			By("creating the initial replication controller")
			framework.RunKubectlOrDieInput(string(nautilus[:]), "create", "-f", "-", fmt.Sprintf("--namespace=%v", ns))
			framework.ValidateController(c, nautilusImage, 2, "update-demo", updateDemoSelector, getUDData("nautilus.jpg", ns), ns)
			By("rolling-update to new replication controller")
			framework.RunKubectlOrDieInput(string(kitten[:]), "rolling-update", "update-demo-nautilus", "--update-period=1s", "-f", "-", fmt.Sprintf("--namespace=%v", ns))
			framework.ValidateController(c, kittenImage, 2, "update-demo", updateDemoSelector, getUDData("kitten.jpg", ns), ns)
			// Everything will hopefully be cleaned up when the namespace is deleted.
		})
	})

	framework.KubeDescribe("Guestbook application", func() {
		forEachGBFile := func(run func(s string)) {
			for _, gbAppFile := range []string{
				"examples/guestbook/frontend-deployment.yaml",
				"examples/guestbook/frontend-service.yaml",
				"examples/guestbook/redis-master-deployment.yaml",
				"examples/guestbook/redis-master-service.yaml",
				"examples/guestbook/redis-slave-deployment.yaml",
				"examples/guestbook/redis-slave-service.yaml",
			} {
				contents := framework.ReadOrDie(gbAppFile)
				run(string(contents))
			}
		}

		It("should create and stop a working application [Conformance]", func() {
			framework.SkipUnlessServerVersionGTE(deploymentsVersion, c)

			defer forEachGBFile(func(contents string) {
				cleanupKubectlInputs(contents, ns)
			})
			By("creating all guestbook components")
			forEachGBFile(func(contents string) {
				framework.Logf(contents)
				framework.RunKubectlOrDieInput(contents, "create", "-f", "-", fmt.Sprintf("--namespace=%v", ns))
			})

			By("validating guestbook app")
			validateGuestbookApp(c, ns)
		})
	})

	framework.KubeDescribe("Simple pod", func() {
		var podPath []byte

		BeforeEach(func() {
			podPath = framework.ReadOrDie(path.Join(kubeCtlManifestPath, "pod-with-readiness-probe.yaml"))
			By(fmt.Sprintf("creating the pod from %v", string(podPath)))
			framework.RunKubectlOrDieInput(string(podPath[:]), "create", "-f", "-", fmt.Sprintf("--namespace=%v", ns))
			Expect(framework.CheckPodsRunningReady(c, ns, []string{simplePodName}, framework.PodStartTimeout)).To(BeTrue())
		})
		AfterEach(func() {
			cleanupKubectlInputs(string(podPath[:]), ns, simplePodSelector)
		})

		It("should support exec", func() {
			By("executing a command in the container")
			execOutput := framework.RunKubectlOrDie("exec", fmt.Sprintf("--namespace=%v", ns), simplePodName, "echo", "running", "in", "container")
			if e, a := "running in container", strings.TrimSpace(execOutput); e != a {
				framework.Failf("Unexpected kubectl exec output. Wanted %q, got %q", e, a)
			}

			By("executing a command in the container with noninteractive stdin")
			execOutput = framework.NewKubectlCommand("exec", fmt.Sprintf("--namespace=%v", ns), "-i", simplePodName, "cat").
				WithStdinData("abcd1234").
				ExecOrDie()
			if e, a := "abcd1234", execOutput; e != a {
				framework.Failf("Unexpected kubectl exec output. Wanted %q, got %q", e, a)
			}

			// pretend that we're a user in an interactive shell
			r, closer, err := newBlockingReader("echo hi\nexit\n")
			if err != nil {
				framework.Failf("Error creating blocking reader: %v", err)
			}
			// NOTE this is solely for test cleanup!
			defer closer.Close()

			By("executing a command in the container with pseudo-interactive stdin")
			execOutput = framework.NewKubectlCommand("exec", fmt.Sprintf("--namespace=%v", ns), "-i", simplePodName, "bash").
				WithStdinReader(r).
				ExecOrDie()
			if e, a := "hi", strings.TrimSpace(execOutput); e != a {
				framework.Failf("Unexpected kubectl exec output. Wanted %q, got %q", e, a)
			}
		})

		It("should support exec through an HTTP proxy", func() {
			// Note: We are skipping local since we want to verify an apiserver with HTTPS.
			// At this time local only supports plain HTTP.
			framework.SkipIfProviderIs("local")
			// Fail if the variable isn't set
			if framework.TestContext.Host == "" {
				framework.Failf("--host variable must be set to the full URI to the api server on e2e run.")
			}

			By("Starting goproxy")
			testSrv, proxyLogs := startLocalProxy()
			defer testSrv.Close()
			proxyAddr := testSrv.URL

			for _, proxyVar := range []string{"https_proxy", "HTTPS_PROXY"} {
				proxyLogs.Reset()
				By("Running kubectl via an HTTP proxy using " + proxyVar)
				output := framework.NewKubectlCommand(fmt.Sprintf("--namespace=%s", ns), "exec", "nginx", "echo", "running", "in", "container").
					WithEnv(append(os.Environ(), fmt.Sprintf("%s=%s", proxyVar, proxyAddr))).
					ExecOrDie()

				// Verify we got the normal output captured by the exec server
				expectedExecOutput := "running in container\n"
				if output != expectedExecOutput {
					framework.Failf("Unexpected kubectl exec output. Wanted %q, got  %q", expectedExecOutput, output)
				}

				// Verify the proxy server logs saw the connection
				expectedProxyLog := fmt.Sprintf("Accepting CONNECT to %s", strings.TrimRight(strings.TrimLeft(framework.TestContext.Host, "https://"), "/api"))

				proxyLog := proxyLogs.String()
				if !strings.Contains(proxyLog, expectedProxyLog) {
					framework.Failf("Missing expected log result on proxy server for %s. Expected: %q, got %q", proxyVar, expectedProxyLog, proxyLog)
				}
			}
		})

		It("should support inline execution and attach", func() {
			framework.SkipUnlessServerVersionGTE(jobsVersion, c)

			nsFlag := fmt.Sprintf("--namespace=%v", ns)

			By("executing a command with run and attach with stdin")
			runOutput := framework.NewKubectlCommand(nsFlag, "run", "run-test", "--image="+busyboxImage, "--restart=OnFailure", "--attach=true", "--stdin", "--", "sh", "-c", "cat && echo 'stdin closed'").
				WithStdinData("abcd1234").
				ExecOrDie()
			Expect(runOutput).To(ContainSubstring("abcd1234"))
			Expect(runOutput).To(ContainSubstring("stdin closed"))
			Expect(c.Extensions().Jobs(ns).Delete("run-test", nil)).To(BeNil())

			By("executing a command with run and attach without stdin")
			runOutput = framework.NewKubectlCommand(fmt.Sprintf("--namespace=%v", ns), "run", "run-test-2", "--image="+busyboxImage, "--restart=OnFailure", "--attach=true", "--leave-stdin-open=true", "--", "sh", "-c", "cat && echo 'stdin closed'").
				WithStdinData("abcd1234").
				ExecOrDie()
			Expect(runOutput).ToNot(ContainSubstring("abcd1234"))
			Expect(runOutput).To(ContainSubstring("stdin closed"))
			Expect(c.Extensions().Jobs(ns).Delete("run-test-2", nil)).To(BeNil())

			By("executing a command with run and attach with stdin with open stdin should remain running")
			runOutput = framework.NewKubectlCommand(nsFlag, "run", "run-test-3", "--image="+busyboxImage, "--restart=OnFailure", "--attach=true", "--leave-stdin-open=true", "--stdin", "--", "sh", "-c", "cat && echo 'stdin closed'").
				WithStdinData("abcd1234\n").
				ExecOrDie()
			Expect(runOutput).ToNot(ContainSubstring("stdin closed"))
			f := func(pods []*api.Pod) sort.Interface { return sort.Reverse(controller.ActivePods(pods)) }
			runTestPod, _, err := util.GetFirstPod(c, ns, labels.SelectorFromSet(map[string]string{"run": "run-test-3"}), 1*time.Minute, f)
			if err != nil {
				os.Exit(1)
			}
			if !framework.CheckPodsRunningReady(c, ns, []string{runTestPod.Name}, time.Minute) {
				framework.Failf("Pod %q of Job %q should still be running", runTestPod.Name, "run-test-3")
			}

			// NOTE: we cannot guarantee our output showed up in the container logs before stdin was closed, so we have
			// to loop test.
			err = wait.PollImmediate(time.Second, time.Minute, func() (bool, error) {
				if !framework.CheckPodsRunningReady(c, ns, []string{runTestPod.Name}, 1*time.Second) {
					framework.Failf("Pod %q of Job %q should still be running", runTestPod.Name, "run-test-3")
				}
				logOutput := framework.RunKubectlOrDie(nsFlag, "logs", runTestPod.Name)
				Expect(logOutput).ToNot(ContainSubstring("stdin closed"))
				return strings.Contains(logOutput, "abcd1234"), nil
			})
			if err != nil {
				os.Exit(1)
			}
			Expect(err).To(BeNil())

			Expect(c.Extensions().Jobs(ns).Delete("run-test-3", nil)).To(BeNil())
		})

		It("should support port-forward", func() {
			By("forwarding the container port to a local port")
			cmd := runPortForward(ns, simplePodName, simplePodPort)
			defer cmd.Stop()

			By("curling local port output")
			localAddr := fmt.Sprintf("http://localhost:%d", cmd.port)
			body, err := curl(localAddr)
			framework.Logf("got: %s", body)
			if err != nil {
				framework.Failf("Failed http.Get of forwarded port (%s): %v", localAddr, err)
			}
			if !strings.Contains(body, nginxDefaultOutput) {
				framework.Failf("Container port output missing expected value. Wanted:'%s', got: %s", nginxDefaultOutput, body)
			}
		})
	})

	framework.KubeDescribe("Kubectl api-versions", func() {
		It("should check if v1 is in available api versions [Conformance]", func() {
			By("validating api verions")
			output := framework.RunKubectlOrDie("api-versions")
			if !strings.Contains(output, "v1") {
				framework.Failf("No v1 in kubectl api-versions")
			}
		})
	})

	framework.KubeDescribe("Kubectl apply", func() {
		It("should apply a new configuration to an existing RC", func() {
			controllerJson := readTestFileOrDie(redisControllerFilename)

			nsFlag := fmt.Sprintf("--namespace=%v", ns)
			By("creating Redis RC")
			framework.RunKubectlOrDieInput(string(controllerJson), "create", "-f", "-", nsFlag)
			By("applying a modified configuration")
			stdin := modifyReplicationControllerConfiguration(string(controllerJson))
			framework.NewKubectlCommand("apply", "-f", "-", nsFlag).
				WithStdinReader(stdin).
				ExecOrDie()
			By("checking the result")
			forEachReplicationController(c, ns, "app", "redis", validateReplicationControllerConfiguration)
		})
		It("should reuse nodePort when apply to an existing SVC", func() {
			serviceJson := readTestFileOrDie(redisServiceFilename)
			nsFlag := fmt.Sprintf("--namespace=%v", ns)

			By("creating Redis SVC")
			framework.RunKubectlOrDieInput(string(serviceJson[:]), "create", "-f", "-", nsFlag)

			By("getting the original nodePort")
			originalNodePort := framework.RunKubectlOrDie("get", "service", "redis-master", nsFlag, "-o", "jsonpath={.spec.ports[0].nodePort}")

			By("applying the same configuration")
			framework.RunKubectlOrDieInput(string(serviceJson[:]), "apply", "-f", "-", nsFlag)

			By("getting the nodePort after applying configuration")
			currentNodePort := framework.RunKubectlOrDie("get", "service", "redis-master", nsFlag, "-o", "jsonpath={.spec.ports[0].nodePort}")

			By("checking the result")
			if originalNodePort != currentNodePort {
				framework.Failf("nodePort should keep the same")
			}
		})
	})

	framework.KubeDescribe("Kubectl cluster-info", func() {
		It("should check if Kubernetes master services is included in cluster-info [Conformance]", func() {
			By("validating cluster-info")
			output := framework.RunKubectlOrDie("cluster-info")
			// Can't check exact strings due to terminal control commands (colors)
			requiredItems := []string{"Kubernetes master", "is running at"}
			if framework.ProviderIs("gce", "gke") {
				requiredItems = append(requiredItems, "KubeDNS", "Heapster")
			}
			for _, item := range requiredItems {
				if !strings.Contains(output, item) {
					framework.Failf("Missing %s in kubectl cluster-info", item)
				}
			}
		})
	})

	framework.KubeDescribe("Kubectl describe", func() {
		// Flaky issue: #25083
		It("should check if kubectl describe prints relevant information for rc and pods [Conformance] [Flaky]", func() {
			framework.SkipUnlessServerVersionGTE(nodePortsOptionalVersion, c)
			controllerJson := readTestFileOrDie(redisControllerFilename)
			serviceJson := readTestFileOrDie(redisServiceFilename)

			nsFlag := fmt.Sprintf("--namespace=%v", ns)
			framework.RunKubectlOrDieInput(string(controllerJson[:]), "create", "-f", "-", nsFlag)
			framework.RunKubectlOrDieInput(string(serviceJson[:]), "create", "-f", "-", nsFlag)

			By("Waiting for Redis master to start.")
			waitForOrFailWithDebug(1)
			// Pod
			forEachPod(func(pod api.Pod) {
				output := framework.RunKubectlOrDie("describe", "pod", pod.Name, nsFlag)
				requiredStrings := [][]string{
					{"Name:", "redis-master-"},
					{"Namespace:", ns},
					{"Node:"},
					{"Labels:", "app=redis"},
					{"role=master"},
					{"Status:", "Running"},
					{"IP:"},
					{"Controllers:", "ReplicationController/redis-master"},
					{"Image:", redisImage},
					{"cpu:", "BestEffort"},
					{"State:", "Running"},
				}
				checkOutput(output, requiredStrings)
			})

			// Rc
			output := framework.RunKubectlOrDie("describe", "rc", "redis-master", nsFlag)
			requiredStrings := [][]string{
				{"Name:", "redis-master"},
				{"Namespace:", ns},
				{"Image(s):", redisImage},
				{"Selector:", "app=redis,role=master"},
				{"Labels:", "app=redis"},
				{"role=master"},
				{"Replicas:", "1 current", "1 desired"},
				{"Pods Status:", "1 Running", "0 Waiting", "0 Succeeded", "0 Failed"},
				// {"Events:"} would ordinarily go in the list
				// here, but in some rare circumstances the
				// events are delayed, and instead kubectl
				// prints "No events." This string will match
				// either way.
				{"vents"}}
			checkOutput(output, requiredStrings)

			// Service
			output = framework.RunKubectlOrDie("describe", "service", "redis-master", nsFlag)
			requiredStrings = [][]string{
				{"Name:", "redis-master"},
				{"Namespace:", ns},
				{"Labels:", "app=redis"},
				{"role=master"},
				{"Selector:", "app=redis", "role=master"},
				{"Type:", "ClusterIP"},
				{"IP:"},
				{"Port:", "<unset>", "6379/TCP"},
				{"Endpoints:"},
				{"Session Affinity:", "None"}}
			checkOutput(output, requiredStrings)

			// Node
			// It should be OK to list unschedulable Nodes here.
			nodes, err := c.Nodes().List(api.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			node := nodes.Items[0]
			output = framework.RunKubectlOrDie("describe", "node", node.Name)
			requiredStrings = [][]string{
				{"Name:", node.Name},
				{"Labels:"},
				{"CreationTimestamp:"},
				{"Conditions:"},
				{"Type", "Status", "LastHeartbeatTime", "LastTransitionTime", "Reason", "Message"},
				{"Addresses:"},
				{"Capacity:"},
				{"Version:"},
				{"Kernel Version:"},
				{"OS Image:"},
				{"Container Runtime Version:"},
				{"Kubelet Version:"},
				{"Kube-Proxy Version:"},
				{"Pods:"}}
			checkOutput(output, requiredStrings)

			// Namespace
			output = framework.RunKubectlOrDie("describe", "namespace", ns)
			requiredStrings = [][]string{
				{"Name:", ns},
				{"Labels:"},
				{"Status:", "Active"}}
			checkOutput(output, requiredStrings)

			// Quota and limitrange are skipped for now.
		})
	})

	framework.KubeDescribe("Kubectl expose", func() {
		It("should create services for rc [Conformance]", func() {
			controllerJson := readTestFileOrDie(redisControllerFilename)
			nsFlag := fmt.Sprintf("--namespace=%v", ns)

			redisPort := 6379

			By("creating Redis RC")

			framework.Logf("namespace %v", ns)
			framework.RunKubectlOrDieInput(string(controllerJson[:]), "create", "-f", "-", nsFlag)

			// It may take a while for the pods to get registered in some cases, wait to be sure.
			By("Waiting for Redis master to start.")
			waitForOrFailWithDebug(1)
			forEachPod(func(pod api.Pod) {
				framework.Logf("wait on redis-master startup in %v ", ns)
				framework.LookForStringInLog(ns, pod.Name, "redis-master", "The server is now ready to accept connections", framework.PodStartTimeout)
			})
			validateService := func(name string, servicePort int, timeout time.Duration) {
				err := wait.Poll(framework.Poll, timeout, func() (bool, error) {
					endpoints, err := c.Endpoints(ns).Get(name)
					if err != nil {
						if apierrs.IsNotFound(err) {
							err = nil
						}
						framework.Logf("Get endpoints failed (interval %v): %v", framework.Poll, err)
						return false, err
					}

					uidToPort := getContainerPortsByPodUID(endpoints)
					if len(uidToPort) == 0 {
						framework.Logf("No endpoint found, retrying")
						return false, nil
					}
					if len(uidToPort) > 1 {
						Fail("Too many endpoints found")
					}
					for _, port := range uidToPort {
						if port[0] != redisPort {
							framework.Failf("Wrong endpoint port: %d", port[0])
						}
					}
					return true, nil
				})
				Expect(err).NotTo(HaveOccurred())

				service, err := c.Services(ns).Get(name)
				Expect(err).NotTo(HaveOccurred())

				if len(service.Spec.Ports) != 1 {
					framework.Failf("1 port is expected")
				}
				port := service.Spec.Ports[0]
				if port.Port != int32(servicePort) {
					framework.Failf("Wrong service port: %d", port.Port)
				}
				if port.TargetPort.IntValue() != redisPort {
					framework.Failf("Wrong target port: %d")
				}
			}

			By("exposing RC")
			framework.RunKubectlOrDie("expose", "rc", "redis-master", "--name=rm2", "--port=1234", fmt.Sprintf("--target-port=%d", redisPort), nsFlag)
			framework.WaitForService(c, ns, "rm2", true, framework.Poll, framework.ServiceStartTimeout)
			validateService("rm2", 1234, framework.ServiceStartTimeout)

			By("exposing service")
			framework.RunKubectlOrDie("expose", "service", "rm2", "--name=rm3", "--port=2345", fmt.Sprintf("--target-port=%d", redisPort), nsFlag)
			framework.WaitForService(c, ns, "rm3", true, framework.Poll, framework.ServiceStartTimeout)
			validateService("rm3", 2345, framework.ServiceStartTimeout)
		})
	})

	framework.KubeDescribe("Kubectl label", func() {
		var pod []byte
		var nsFlag string
		BeforeEach(func() {
			pod = readTestFileOrDie("pause-pod.yaml")
			By("creating the pod")
			nsFlag = fmt.Sprintf("--namespace=%v", ns)
			framework.RunKubectlOrDieInput(string(pod), "create", "-f", "-", nsFlag)
			Expect(framework.CheckPodsRunningReady(c, ns, []string{pausePodName}, framework.PodStartTimeout)).To(BeTrue())
		})
		AfterEach(func() {
			cleanupKubectlInputs(string(pod[:]), ns, pausePodSelector)
		})

		It("should update the label on a resource [Conformance]", func() {
			labelName := "testing-label"
			labelValue := "testing-label-value"

			By("adding the label " + labelName + " with value " + labelValue + " to a pod")
			framework.RunKubectlOrDie("label", "pods", pausePodName, labelName+"="+labelValue, nsFlag)
			By("verifying the pod has the label " + labelName + " with the value " + labelValue)
			output := framework.RunKubectlOrDie("get", "pod", pausePodName, "-L", labelName, nsFlag)
			if !strings.Contains(output, labelValue) {
				framework.Failf("Failed updating label " + labelName + " to the pod " + pausePodName)
			}

			By("removing the label " + labelName + " of a pod")
			framework.RunKubectlOrDie("label", "pods", pausePodName, labelName+"-", nsFlag)
			By("verifying the pod doesn't have the label " + labelName)
			output = framework.RunKubectlOrDie("get", "pod", pausePodName, "-L", labelName, nsFlag)
			if strings.Contains(output, labelValue) {
				framework.Failf("Failed removing label " + labelName + " of the pod " + pausePodName)
			}
		})
	})

	framework.KubeDescribe("Kubectl logs", func() {
		var rc []byte
		var nsFlag string
		containerName := "redis-master"
		BeforeEach(func() {
			rc = readTestFileOrDie(redisControllerFilename)
			By("creating an rc")
			nsFlag = fmt.Sprintf("--namespace=%v", ns)
			framework.RunKubectlOrDieInput(string(rc[:]), "create", "-f", "-", nsFlag)
		})
		AfterEach(func() {
			cleanupKubectlInputs(string(rc[:]), ns, simplePodSelector)
		})

		It("should be able to retrieve and filter logs [Conformance]", func() {
			framework.SkipUnlessServerVersionGTE(extendedPodLogFilterVersion, c)

			// Split("something\n", "\n") returns ["something", ""], so
			// strip trailing newline first
			lines := func(out string) []string {
				return strings.Split(strings.TrimRight(out, "\n"), "\n")
			}

			By("Waiting for Redis master to start.")
			waitForOrFailWithDebug(1)
			forEachPod(func(pod api.Pod) {
				By("checking for a matching strings")
				_, err := framework.LookForStringInLog(ns, pod.Name, containerName, "The server is now ready to accept connections", framework.PodStartTimeout)
				Expect(err).NotTo(HaveOccurred())

				By("limiting log lines")
				out := framework.RunKubectlOrDie("log", pod.Name, containerName, nsFlag, "--tail=1")
				Expect(len(out)).NotTo(BeZero())
				Expect(len(lines(out))).To(Equal(1))

				By("limiting log bytes")
				out = framework.RunKubectlOrDie("log", pod.Name, containerName, nsFlag, "--limit-bytes=1")
				Expect(len(lines(out))).To(Equal(1))
				Expect(len(out)).To(Equal(1))

				By("exposing timestamps")
				out = framework.RunKubectlOrDie("log", pod.Name, containerName, nsFlag, "--tail=1", "--timestamps")
				l := lines(out)
				Expect(len(l)).To(Equal(1))
				words := strings.Split(l[0], " ")
				Expect(len(words)).To(BeNumerically(">", 1))
				if _, err := time.Parse(time.RFC3339Nano, words[0]); err != nil {
					if _, err := time.Parse(time.RFC3339, words[0]); err != nil {
						framework.Failf("expected %q to be RFC3339 or RFC3339Nano", words[0])
					}
				}

				By("restricting to a time range")
				// Note: we must wait at least two seconds,
				// because the granularity is only 1 second and
				// it could end up rounding the wrong way.
				time.Sleep(2500 * time.Millisecond) // ensure that startup logs on the node are seen as older than 1s
				recent_out := framework.RunKubectlOrDie("log", pod.Name, containerName, nsFlag, "--since=1s")
				recent := len(strings.Split(recent_out, "\n"))
				older_out := framework.RunKubectlOrDie("log", pod.Name, containerName, nsFlag, "--since=24h")
				older := len(strings.Split(older_out, "\n"))
				Expect(recent).To(BeNumerically("<", older), "expected recent(%v) to be less than older(%v)\nrecent lines:\n%v\nolder lines:\n%v\n", recent, older, recent_out, older_out)
			})
		})
	})

	framework.KubeDescribe("Kubectl patch", func() {
		It("should add annotations for pods in rc [Conformance]", func() {
			controllerJson := readTestFileOrDie(redisControllerFilename)
			nsFlag := fmt.Sprintf("--namespace=%v", ns)
			By("creating Redis RC")
			framework.RunKubectlOrDieInput(string(controllerJson[:]), "create", "-f", "-", nsFlag)
			By("Waiting for Redis master to start.")
			waitForOrFailWithDebug(1)
			By("patching all pods")
			forEachPod(func(pod api.Pod) {
				framework.RunKubectlOrDie("patch", "pod", pod.Name, nsFlag, "-p", "{\"metadata\":{\"annotations\":{\"x\":\"y\"}}}")
			})

			By("checking annotations")
			forEachPod(func(pod api.Pod) {
				found := false
				for key, val := range pod.Annotations {
					if key == "x" && val == "y" {
						found = true
					}
				}
				if !found {
					framework.Failf("Added annotation not found")
				}
			})
		})
	})

	framework.KubeDescribe("Kubectl version", func() {
		It("should check is all data is printed [Conformance]", func() {
			version := framework.RunKubectlOrDie("version")
			requiredItems := []string{"Client Version:", "Server Version:", "Major:", "Minor:", "GitCommit:"}
			for _, item := range requiredItems {
				if !strings.Contains(version, item) {
					framework.Failf("Required item %s not found in %s", item, version)
				}
			}
		})
	})

	framework.KubeDescribe("Kubectl run default", func() {
		var nsFlag string
		var name string

		var cleanUp func()

		BeforeEach(func() {
			nsFlag = fmt.Sprintf("--namespace=%v", ns)
			gte, err := framework.ServerVersionGTE(deploymentsVersion, c)
			if err != nil {
				framework.Failf("Failed to get server version: %v", err)
			}
			if gte {
				name = "e2e-test-nginx-deployment"
				cleanUp = func() { framework.RunKubectlOrDie("delete", "deployment", name, nsFlag) }
			} else {
				name = "e2e-test-nginx-rc"
				cleanUp = func() { framework.RunKubectlOrDie("delete", "rc", name, nsFlag) }
			}
		})

		AfterEach(func() {
			cleanUp()
		})

		It("should create an rc or deployment from an image [Conformance]", func() {
			By("running the image " + nginxImage)
			framework.RunKubectlOrDie("run", name, "--image="+nginxImage, nsFlag)
			By("verifying the pod controlled by " + name + " gets created")
			label := labels.SelectorFromSet(labels.Set(map[string]string{"run": name}))
			podlist, err := framework.WaitForPodsWithLabel(c, ns, label)
			if err != nil {
				framework.Failf("Failed getting pod controlled by %s: %v", name, err)
			}
			pods := podlist.Items
			if pods == nil || len(pods) != 1 || len(pods[0].Spec.Containers) != 1 || pods[0].Spec.Containers[0].Image != nginxImage {
				framework.RunKubectlOrDie("get", "pods", "-L", "run", nsFlag)
				framework.Failf("Failed creating 1 pod with expected image %s. Number of pods = %v", nginxImage, len(pods))
			}
		})
	})

	framework.KubeDescribe("Kubectl run rc", func() {
		var nsFlag string
		var rcName string

		BeforeEach(func() {
			nsFlag = fmt.Sprintf("--namespace=%v", ns)
			rcName = "e2e-test-nginx-rc"
		})

		AfterEach(func() {
			framework.RunKubectlOrDie("delete", "rc", rcName, nsFlag)
		})

		It("should create an rc from an image [Conformance]", func() {
			By("running the image " + nginxImage)
			framework.RunKubectlOrDie("run", rcName, "--image="+nginxImage, "--generator=run/v1", nsFlag)
			By("verifying the rc " + rcName + " was created")
			rc, err := c.ReplicationControllers(ns).Get(rcName)
			if err != nil {
				framework.Failf("Failed getting rc %s: %v", rcName, err)
			}
			containers := rc.Spec.Template.Spec.Containers
			if containers == nil || len(containers) != 1 || containers[0].Image != nginxImage {
				framework.Failf("Failed creating rc %s for 1 pod with expected image %s", rcName, nginxImage)
			}

			By("verifying the pod controlled by rc " + rcName + " was created")
			label := labels.SelectorFromSet(labels.Set(map[string]string{"run": rcName}))
			podlist, err := framework.WaitForPodsWithLabel(c, ns, label)
			if err != nil {
				framework.Failf("Failed getting pod controlled by rc %s: %v", rcName, err)
			}
			pods := podlist.Items
			if pods == nil || len(pods) != 1 || len(pods[0].Spec.Containers) != 1 || pods[0].Spec.Containers[0].Image != nginxImage {
				framework.RunKubectlOrDie("get", "pods", "-L", "run", nsFlag)
				framework.Failf("Failed creating 1 pod with expected image %s. Number of pods = %v", nginxImage, len(pods))
			}

			By("confirm that you can get logs from an rc")
			podNames := []string{}
			for _, pod := range pods {
				podNames = append(podNames, pod.Name)
			}
			if !framework.CheckPodsRunningReady(c, ns, podNames, framework.PodStartTimeout) {
				framework.Failf("Pods for rc %s were not ready", rcName)
			}
			_, err = framework.RunKubectl("logs", "rc/"+rcName, nsFlag)
			// a non-nil error is fine as long as we actually found a pod.
			if err != nil && !strings.Contains(err.Error(), " in pod ") {
				framework.Failf("Failed getting logs by rc %s: %v", rcName, err)
			}
		})
	})

	framework.KubeDescribe("Kubectl rolling-update", func() {
		var nsFlag string
		var rcName string
		var c *client.Client

		BeforeEach(func() {
			c = f.Client
			nsFlag = fmt.Sprintf("--namespace=%v", ns)
			rcName = "e2e-test-nginx-rc"
		})

		AfterEach(func() {
			framework.RunKubectlOrDie("delete", "rc", rcName, nsFlag)
		})

		It("should support rolling-update to same image [Conformance]", func() {
			By("running the image " + nginxImage)
			framework.RunKubectlOrDie("run", rcName, "--image="+nginxImage, "--generator=run/v1", nsFlag)
			By("verifying the rc " + rcName + " was created")
			rc, err := c.ReplicationControllers(ns).Get(rcName)
			if err != nil {
				framework.Failf("Failed getting rc %s: %v", rcName, err)
			}
			containers := rc.Spec.Template.Spec.Containers
			if containers == nil || len(containers) != 1 || containers[0].Image != nginxImage {
				framework.Failf("Failed creating rc %s for 1 pod with expected image %s", rcName, nginxImage)
			}
			framework.WaitForRCToStabilize(c, ns, rcName, framework.PodStartTimeout)

			By("rolling-update to same image controller")

			runKubectlRetryOrDie("rolling-update", rcName, "--update-period=1s", "--image="+nginxImage, "--image-pull-policy="+string(api.PullIfNotPresent), nsFlag)
			framework.ValidateController(c, nginxImage, 1, rcName, "run="+rcName, noOpValidatorFn, ns)
		})
	})

	framework.KubeDescribe("Kubectl run deployment", func() {
		var nsFlag string
		var dName string

		BeforeEach(func() {
			nsFlag = fmt.Sprintf("--namespace=%v", ns)
			dName = "e2e-test-nginx-deployment"
		})

		AfterEach(func() {
			framework.RunKubectlOrDie("delete", "deployment", dName, nsFlag)
		})

		It("should create a deployment from an image [Conformance]", func() {
			framework.SkipUnlessServerVersionGTE(deploymentsVersion, c)

			By("running the image " + nginxImage)
			framework.RunKubectlOrDie("run", dName, "--image="+nginxImage, "--generator=deployment/v1beta1", nsFlag)
			By("verifying the deployment " + dName + " was created")
			d, err := c.Extensions().Deployments(ns).Get(dName)
			if err != nil {
				framework.Failf("Failed getting deployment %s: %v", dName, err)
			}
			containers := d.Spec.Template.Spec.Containers
			if containers == nil || len(containers) != 1 || containers[0].Image != nginxImage {
				framework.Failf("Failed creating deployment %s for 1 pod with expected image %s", dName, nginxImage)
			}

			By("verifying the pod controlled by deployment " + dName + " was created")
			label := labels.SelectorFromSet(labels.Set(map[string]string{"run": dName}))
			podlist, err := framework.WaitForPodsWithLabel(c, ns, label)
			if err != nil {
				framework.Failf("Failed getting pod controlled by deployment %s: %v", dName, err)
			}
			pods := podlist.Items
			if pods == nil || len(pods) != 1 || len(pods[0].Spec.Containers) != 1 || pods[0].Spec.Containers[0].Image != nginxImage {
				framework.RunKubectlOrDie("get", "pods", "-L", "run", nsFlag)
				framework.Failf("Failed creating 1 pod with expected image %s. Number of pods = %v", nginxImage, len(pods))
			}
		})
	})

	framework.KubeDescribe("Kubectl run job", func() {
		var nsFlag string
		var jobName string

		BeforeEach(func() {
			nsFlag = fmt.Sprintf("--namespace=%v", ns)
			jobName = "e2e-test-nginx-job"
		})

		AfterEach(func() {
			framework.RunKubectlOrDie("delete", "jobs", jobName, nsFlag)
		})

		It("should create a job from an image when restart is OnFailure [Conformance]", func() {
			framework.SkipUnlessServerVersionGTE(jobsVersion, c)

			By("running the image " + nginxImage)
			framework.RunKubectlOrDie("run", jobName, "--restart=OnFailure", "--generator=job/v1", "--image="+nginxImage, nsFlag)
			By("verifying the job " + jobName + " was created")
			job, err := c.Extensions().Jobs(ns).Get(jobName)
			if err != nil {
				framework.Failf("Failed getting job %s: %v", jobName, err)
			}
			containers := job.Spec.Template.Spec.Containers
			if containers == nil || len(containers) != 1 || containers[0].Image != nginxImage {
				framework.Failf("Failed creating job %s for 1 pod with expected image %s", jobName, nginxImage)
			}
			if job.Spec.Template.Spec.RestartPolicy != api.RestartPolicyOnFailure {
				framework.Failf("Failed creating a job with correct restart policy for --restart=OnFailure")
			}
		})
	})

	framework.KubeDescribe("Kubectl run pod", func() {
		var nsFlag string
		var podName string

		BeforeEach(func() {
			nsFlag = fmt.Sprintf("--namespace=%v", ns)
			podName = "e2e-test-nginx-pod"
		})

		AfterEach(func() {
			framework.RunKubectlOrDie("delete", "pods", podName, nsFlag)
		})

		It("should create a pod from an image when restart is Never [Conformance]", func() {
			framework.SkipUnlessServerVersionGTE(jobsVersion, c)

			By("running the image " + nginxImage)
			framework.RunKubectlOrDie("run", podName, "--restart=Never", "--generator=run-pod/v1", "--image="+nginxImage, nsFlag)
			By("verifying the pod " + podName + " was created")
			pod, err := c.Pods(ns).Get(podName)
			if err != nil {
				framework.Failf("Failed getting pod %s: %v", podName, err)
			}
			containers := pod.Spec.Containers
			if containers == nil || len(containers) != 1 || containers[0].Image != nginxImage {
				framework.Failf("Failed creating pod %s with expected image %s", podName, nginxImage)
			}
			if pod.Spec.RestartPolicy != api.RestartPolicyNever {
				framework.Failf("Failed creating a pod with correct restart policy for --restart=Never")
			}
		})
	})

	framework.KubeDescribe("Kubectl run --rm job", func() {
		nsFlag := fmt.Sprintf("--namespace=%v", ns)
		jobName := "e2e-test-rm-busybox-job"

		It("should create a job from an image, then delete the job [Conformance]", func() {
			framework.SkipUnlessServerVersionGTE(jobsVersion, c)

			By("executing a command with run --rm and attach with stdin")
			t := time.NewTimer(runJobTimeout)
			defer t.Stop()
			runOutput := framework.NewKubectlCommand(nsFlag, "run", jobName, "--image="+busyboxImage, "--rm=true", "--generator=job/v1", "--restart=OnFailure", "--attach=true", "--stdin", "--", "sh", "-c", "cat && echo 'stdin closed'").
				WithStdinData("abcd1234").
				WithTimeout(t.C).
				ExecOrDie()
			Expect(runOutput).To(ContainSubstring("abcd1234"))
			Expect(runOutput).To(ContainSubstring("stdin closed"))

			By("verifying the job " + jobName + " was deleted")
			_, err := c.Extensions().Jobs(ns).Get(jobName)
			Expect(err).To(HaveOccurred())
			Expect(apierrs.IsNotFound(err)).To(BeTrue())
		})
	})

	framework.KubeDescribe("Proxy server", func() {
		// TODO: test proxy options (static, prefix, etc)
		It("should support proxy with --port 0 [Conformance]", func() {
			By("starting the proxy server")
			port, cmd, err := startProxyServer()
			if cmd != nil {
				defer framework.TryKill(cmd)
			}
			if err != nil {
				framework.Failf("Failed to start proxy server: %v", err)
			}
			By("curling proxy /api/ output")
			localAddr := fmt.Sprintf("http://localhost:%d/api/", port)
			apiVersions, err := getAPIVersions(localAddr)
			if err != nil {
				framework.Failf("Expected at least one supported apiversion, got error %v", err)
			}
			if len(apiVersions.Versions) < 1 {
				framework.Failf("Expected at least one supported apiversion, got %v", apiVersions)
			}
		})

		It("should support --unix-socket=/path [Conformance]", func() {
			By("Starting the proxy")
			tmpdir, err := ioutil.TempDir("", "kubectl-proxy-unix")
			if err != nil {
				framework.Failf("Failed to create temporary directory: %v", err)
			}
			path := filepath.Join(tmpdir, "test")
			defer os.Remove(path)
			defer os.Remove(tmpdir)
			cmd := framework.KubectlCmd("proxy", fmt.Sprintf("--unix-socket=%s", path))
			stdout, stderr, err := framework.StartCmdAndStreamOutput(cmd)
			if err != nil {
				framework.Failf("Failed to start kubectl command: %v", err)
			}
			defer stdout.Close()
			defer stderr.Close()
			defer framework.TryKill(cmd)
			buf := make([]byte, 128)
			if _, err = stdout.Read(buf); err != nil {
				framework.Failf("Expected output from kubectl proxy: %v", err)
			}
			By("retrieving proxy /api/ output")
			_, err = curlUnix("http://unused/api", path)
			if err != nil {
				framework.Failf("Failed get of /api at %s: %v", path, err)
			}
		})
	})

	framework.KubeDescribe("Kubectl taint", func() {
		It("should update the taint on a node", func() {
			taintName := fmt.Sprintf("kubernetes.io/e2e-taint-key-%s", string(pkgutil.NewUUID()))
			taintValue := "testing-taint-value"
			taintEffect := fmt.Sprintf("%s", api.TaintEffectNoSchedule)

			nodes, err := c.Nodes().List(api.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			node := nodes.Items[0]
			nodeName := node.Name

			By("adding the taint " + taintName + " with value " + taintValue + " and taint effect " + taintEffect + " to a node")
			framework.RunKubectlOrDie("taint", "nodes", nodeName, taintName+"="+taintValue+":"+taintEffect)
			By("verifying the node has the taint " + taintName + " with the value " + taintValue)
			output := framework.RunKubectlOrDie("describe", "node", nodeName)
			requiredStrings := [][]string{
				{"Name:", nodeName},
				{"Taints:"},
				{taintName + "=" + taintValue + ":" + taintEffect},
			}
			checkOutput(output, requiredStrings)

			By("removing the taint " + taintName + " of a node")
			framework.RunKubectlOrDie("taint", "nodes", nodeName, taintName+"-")
			By("verifying the node doesn't have the taint " + taintName)
			output = framework.RunKubectlOrDie("describe", "node", nodeName)
			if strings.Contains(output, taintName) {
				framework.Failf("Failed removing taint " + taintName + " of the node " + nodeName)
			}

		})
	})
})

// Checks whether the output split by line contains the required elements.
func checkOutput(output string, required [][]string) {
	outputLines := strings.Split(output, "\n")
	currentLine := 0
	for _, requirement := range required {
		for currentLine < len(outputLines) && !strings.Contains(outputLines[currentLine], requirement[0]) {
			currentLine++
		}
		if currentLine == len(outputLines) {
			framework.Failf("Failed to find %s in %s", requirement[0], output)
		}
		for _, item := range requirement[1:] {
			if !strings.Contains(outputLines[currentLine], item) {
				framework.Failf("Failed to find %s in %s", item, outputLines[currentLine])
			}
		}
	}
}

func getAPIVersions(apiEndpoint string) (*unversioned.APIVersions, error) {
	body, err := curl(apiEndpoint)
	if err != nil {
		return nil, fmt.Errorf("Failed http.Get of %s: %v", apiEndpoint, err)
	}
	var apiVersions unversioned.APIVersions
	if err := json.Unmarshal([]byte(body), &apiVersions); err != nil {
		return nil, fmt.Errorf("Failed to parse /api output %s: %v", body, err)
	}
	return &apiVersions, nil
}

func startProxyServer() (int, *exec.Cmd, error) {
	// Specifying port 0 indicates we want the os to pick a random port.
	cmd := framework.KubectlCmd("proxy", "-p", "0")
	stdout, stderr, err := framework.StartCmdAndStreamOutput(cmd)
	if err != nil {
		return -1, nil, err
	}
	defer stdout.Close()
	defer stderr.Close()
	buf := make([]byte, 128)
	var n int
	if n, err = stdout.Read(buf); err != nil {
		return -1, cmd, fmt.Errorf("Failed to read from kubectl proxy stdout: %v", err)
	}
	output := string(buf[:n])
	match := proxyRegexp.FindStringSubmatch(output)
	if len(match) == 2 {
		if port, err := strconv.Atoi(match[1]); err == nil {
			return port, cmd, nil
		}
	}
	return -1, cmd, fmt.Errorf("Failed to parse port from proxy stdout: %s", output)
}

func curlUnix(url string, path string) (string, error) {
	dial := func(proto, addr string) (net.Conn, error) {
		return net.Dial("unix", path)
	}
	transport := utilnet.SetTransportDefaults(&http.Transport{
		Dial: dial,
	})
	return curlTransport(url, transport)
}

func curlTransport(url string, transport *http.Transport) (string, error) {
	client := &http.Client{Transport: transport}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body[:]), nil
}

func curl(url string) (string, error) {
	return curlTransport(url, utilnet.SetTransportDefaults(&http.Transport{}))
}

func validateGuestbookApp(c *client.Client, ns string) {
	framework.Logf("Waiting for frontend to serve content.")
	if !waitForGuestbookResponse(c, "get", "", `{"data": ""}`, guestbookStartupTimeout, ns) {
		framework.Failf("Frontend service did not start serving content in %v seconds.", guestbookStartupTimeout.Seconds())
	}

	framework.Logf("Trying to add a new entry to the guestbook.")
	if !waitForGuestbookResponse(c, "set", "TestEntry", `{"message": "Updated"}`, guestbookResponseTimeout, ns) {
		framework.Failf("Cannot added new entry in %v seconds.", guestbookResponseTimeout.Seconds())
	}

	framework.Logf("Verifying that added entry can be retrieved.")
	if !waitForGuestbookResponse(c, "get", "", `{"data": "TestEntry"}`, guestbookResponseTimeout, ns) {
		framework.Failf("Entry to guestbook wasn't correctly added in %v seconds.", guestbookResponseTimeout.Seconds())
	}
}

// Returns whether received expected response from guestbook on time.
func waitForGuestbookResponse(c *client.Client, cmd, arg, expectedResponse string, timeout time.Duration, ns string) bool {
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(5 * time.Second) {
		res, err := makeRequestToGuestbook(c, cmd, arg, ns)
		if err == nil && res == expectedResponse {
			return true
		}
		framework.Logf("Failed to get response from guestbook. err: %v, response: %s", err, res)
	}
	return false
}

func makeRequestToGuestbook(c *client.Client, cmd, value string, ns string) (string, error) {
	proxyRequest, errProxy := framework.GetServicesProxyRequest(c, c.Get())
	if errProxy != nil {
		return "", errProxy
	}
	result, err := proxyRequest.Namespace(ns).
		Name("frontend").
		Suffix("/guestbook.php").
		Param("cmd", cmd).
		Param("key", "messages").
		Param("value", value).
		Do().
		Raw()
	return string(result), err
}

type updateDemoData struct {
	Image string
}

const applyTestLabel = "kubectl.kubernetes.io/apply-test"

func readBytesFromFile(filename string) []byte {
	file, err := os.Open(filename)
	if err != nil {
		framework.Failf(err.Error())
	}

	data, err := ioutil.ReadAll(file)
	if err != nil {
		framework.Failf(err.Error())
	}

	return data
}

func readReplicationControllerFromString(contents string) *api.ReplicationController {
	rc := api.ReplicationController{}
	if err := yaml.Unmarshal([]byte(contents), &rc); err != nil {
		framework.Failf(err.Error())
	}

	return &rc
}

func modifyReplicationControllerConfiguration(contents string) io.Reader {
	rc := readReplicationControllerFromString(contents)
	rc.Labels[applyTestLabel] = "ADDED"
	rc.Spec.Selector[applyTestLabel] = "ADDED"
	rc.Spec.Template.Labels[applyTestLabel] = "ADDED"
	data, err := json.Marshal(rc)
	if err != nil {
		framework.Failf("json marshal failed: %s\n", err)
	}

	return bytes.NewReader(data)
}

func forEachReplicationController(c *client.Client, ns, selectorKey, selectorValue string, fn func(api.ReplicationController)) {
	var rcs *api.ReplicationControllerList
	var err error
	for t := time.Now(); time.Since(t) < framework.PodListTimeout; time.Sleep(framework.Poll) {
		label := labels.SelectorFromSet(labels.Set(map[string]string{selectorKey: selectorValue}))
		options := api.ListOptions{LabelSelector: label}
		rcs, err = c.ReplicationControllers(ns).List(options)
		Expect(err).NotTo(HaveOccurred())
		if len(rcs.Items) > 0 {
			break
		}
	}

	if rcs == nil || len(rcs.Items) == 0 {
		framework.Failf("No replication controllers found")
	}

	for _, rc := range rcs.Items {
		fn(rc)
	}
}

func validateReplicationControllerConfiguration(rc api.ReplicationController) {
	if rc.Name == "redis-master" {
		if _, ok := rc.Annotations[annotations.LastAppliedConfigAnnotation]; !ok {
			framework.Failf("Annotation not found in modified configuration:\n%v\n", rc)
		}

		if value, ok := rc.Labels[applyTestLabel]; !ok || value != "ADDED" {
			framework.Failf("Added label %s not found in modified configuration:\n%v\n", applyTestLabel, rc)
		}
	}
}

// getUDData creates a validator function based on the input string (i.e. kitten.jpg).
// For example, if you send "kitten.jpg", this function verifies that the image jpg = kitten.jpg
// in the container's json field.
func getUDData(jpgExpected string, ns string) func(*client.Client, string) error {

	// getUDData validates data.json in the update-demo (returns nil if data is ok).
	return func(c *client.Client, podID string) error {
		framework.Logf("validating pod %s", podID)
		subResourceProxyAvailable, err := framework.ServerVersionGTE(framework.SubResourcePodProxyVersion, c)
		if err != nil {
			return err
		}
		var body []byte
		if subResourceProxyAvailable {
			body, err = c.Get().
				Namespace(ns).
				Resource("pods").
				SubResource("proxy").
				Name(podID).
				Suffix("data.json").
				Do().
				Raw()
		} else {
			body, err = c.Get().
				Prefix("proxy").
				Namespace(ns).
				Resource("pods").
				Name(podID).
				Suffix("data.json").
				Do().
				Raw()
		}
		if err != nil {
			return err
		}
		framework.Logf("got data: %s", body)
		var data updateDemoData
		if err := json.Unmarshal(body, &data); err != nil {
			return err
		}
		framework.Logf("Unmarshalled json jpg/img => %s , expecting %s .", data, jpgExpected)
		if strings.Contains(data.Image, jpgExpected) {
			return nil
		} else {
			return errors.New(fmt.Sprintf("data served up in container is inaccurate, %s didn't contain %s", data, jpgExpected))
		}
	}
}

func noOpValidatorFn(c *client.Client, podID string) error { return nil }

// newBlockingReader returns a reader that allows reading the given string,
// then blocks until Close() is called on the returned closer.
//
// We're explicitly returning the reader and closer separately, because
// the closer needs to be the *os.File we get from os.Pipe(). This is required
// so the exec of kubectl can pass the underlying file descriptor to the exec
// syscall, instead of creating another os.Pipe and blocking on the io.Copy
// between the source (e.g. stdin) and the write half of the pipe.
func newBlockingReader(s string) (io.Reader, io.Closer, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	w.Write([]byte(s))
	return r, w, nil
}

// newStreamingUpload creates a new http.Request that will stream POST
// a file to a URI.
func newStreamingUpload(filePath string) (*io.PipeReader, *multipart.Writer, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, nil, err
	}

	r, w := io.Pipe()

	postBodyWriter := multipart.NewWriter(w)

	go streamingUpload(file, filepath.Base(filePath), postBodyWriter, w)
	return r, postBodyWriter, err
}

// streamingUpload streams a file via a pipe through a multipart.Writer.
// Generally one should use newStreamingUpload instead of calling this directly.
func streamingUpload(file *os.File, fileName string, postBodyWriter *multipart.Writer, w *io.PipeWriter) {
	defer GinkgoRecover()
	defer file.Close()
	defer w.Close()

	// Set up the form file
	fileWriter, err := postBodyWriter.CreateFormFile("file", fileName)
	if err != nil {
		framework.Failf("Unable to to write file at %s to buffer. Error: %s", fileName, err)
	}

	// Copy kubectl binary into the file writer
	if _, err := io.Copy(fileWriter, file); err != nil {
		framework.Failf("Unable to to copy file at %s into the file writer. Error: %s", fileName, err)
	}

	// Nothing more should be written to this instance of the postBodyWriter
	if err := postBodyWriter.Close(); err != nil {
		framework.Failf("Unable to close the writer for file upload. Error: %s", err)
	}
}

var binPrefixes = []string{
	"_output/dockerized/bin",
	"_output/local/bin",
	"platforms",
}

// findBinary searches through likely paths to find the specified binary.  It
// takes the one that has been built most recently.  Platform should be
// specified as '<os>/<arch>'.  For example: 'linux/amd64'.
func findBinary(binName string, platform string) (string, error) {
	var binTime time.Time
	var binPath string

	for _, pre := range binPrefixes {
		tryPath := path.Join(framework.TestContext.RepoRoot, pre, platform, binName)
		fi, err := os.Stat(tryPath)
		if err != nil {
			continue
		}
		if fi.ModTime().After(binTime) {
			binPath = tryPath
			binTime = fi.ModTime()
		}
	}

	if len(binPath) > 0 {
		return binPath, nil
	}
	return binPath, fmt.Errorf("Could not find %v for %v", binName, platform)
}

func startLocalProxy() (srv *httptest.Server, logs *bytes.Buffer) {
	logs = &bytes.Buffer{}
	p := goproxy.NewProxyHttpServer()
	p.Verbose = true
	p.Logger = log.New(logs, "", 0)
	return httptest.NewServer(p), logs
}
