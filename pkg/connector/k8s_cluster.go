package connector

import (
	"fmt"
	"strings"

	"github.com/datawire/ambassador/pkg/k8s"
	"github.com/datawire/ambassador/pkg/supervisor"
	"github.com/pkg/errors"

	"github.com/datawire/telepresence2/pkg/common"
)

// k8sCluster is a Kubernetes cluster reference
type k8sCluster struct {
	ctx          string
	namespace    string
	srv          string
	kargs        []string
	isBridgeOkay func() bool
	common.ResourceBase
}

// getKubectlArgs returns the kubectl command arguments to run a
// kubectl command with this cluster, including the namespace argument.
func (c *k8sCluster) getKubectlArgs(args ...string) []string {
	return c.kubectlArgs(true, args...)
}

// getKubectlArgsNoNamespace returns the kubectl command arguments to run a
// kubectl command with this cluster, but without the namespace argument.
func (c *k8sCluster) getKubectlArgsNoNamespace(args ...string) []string {
	return c.kubectlArgs(false, args...)
}

func (c *k8sCluster) kubectlArgs(includeNamespace bool, args ...string) []string {
	cmdArgs := make([]string, 0, len(c.kargs)+len(args))
	if c.ctx != "" {
		cmdArgs = append(cmdArgs, "--context", c.ctx)
	}

	if includeNamespace {
		if c.namespace != "" {
			cmdArgs = append(cmdArgs, "--namespace", c.namespace)
		}
	}

	cmdArgs = append(cmdArgs, c.kargs...)
	cmdArgs = append(cmdArgs, args...)
	return cmdArgs
}

// getKubectlCmd returns a Cmd that runs kubectl with the given arguments and
// the appropriate environment to talk to the cluster
func (c *k8sCluster) getKubectlCmd(p *supervisor.Process, args ...string) *supervisor.Cmd {
	return p.Command("kubectl", c.getKubectlArgs(args...)...)
}

// getKubectlCmdNoNamespace returns a Cmd that runs kubectl with the given arguments and
// the appropriate environment to talk to the cluster, but it doesn't supply a namespace
// arg.
func (c *k8sCluster) getKubectlCmdNoNamespace(p *supervisor.Process, args ...string) *supervisor.Cmd {
	return p.Command("kubectl", c.getKubectlArgsNoNamespace(args...)...)
}

// context returns the cluster's context name
func (c *k8sCluster) context() string {
	return c.ctx
}

// server returns the cluster's server configuration
func (c *k8sCluster) server() string {
	return c.srv
}

// setBridgeCheck sets the callable used to check whether the Teleproxy bridge
// is functioning. If this is nil/unset, cluster monitoring checks the cluster
// directly (via kubectl)
func (c *k8sCluster) setBridgeCheck(isBridgeOkay func() bool) {
	c.isBridgeOkay = isBridgeOkay
}

// check for cluster connectivity
func (c *k8sCluster) check(p *supervisor.Process) error {
	// If the bridge is okay then the cluster is okay
	if c.isBridgeOkay != nil && c.isBridgeOkay() {
		return nil
	}
	cmd := c.getKubectlCmd(p, "get", "po", "ohai", "--ignore-not-found")
	return cmd.Run()
}

// trackKCluster tracks connectivity to a cluster
func trackKCluster(
	p *supervisor.Process, context, namespace string, kargs []string,
) (*k8sCluster, error) {
	c := &k8sCluster{
		kargs:     kargs,
		ctx:       context,
		namespace: namespace,
	}
	if err := c.check(p); err != nil {
		return nil, errors.Wrap(err, "initial cluster check")
	}

	if c.ctx == "" {
		cmd := c.getKubectlCmd(p, "config", "current-context")
		p.Logf("%s %v", cmd.Path, cmd.Args[1:])
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, errors.Wrap(err, "kubectl config current-context")
		}
		c.ctx = strings.TrimSpace(string(output))
	}
	p.Logf("Context: %s", c.ctx)

	if c.namespace == "" {
		nsQuery := fmt.Sprintf("jsonpath={.contexts[?(@.name==\"%s\")].context.namespace}", c.ctx)
		cmd := c.getKubectlCmd(p, "config", "view", "-o", nsQuery)
		p.Logf("%s %v", cmd.Path, cmd.Args[1:])
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, errors.Wrap(err, "kubectl config view ns")
		}
		c.namespace = strings.TrimSpace(string(output))
		if c.namespace == "" { // This is what kubens does
			c.namespace = "default"
		}
	}
	p.Logf("Namespace: %s", c.namespace)

	cmd := c.getKubectlCmd(p, "config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}")
	p.Logf("%s %v", cmd.Path, cmd.Args[1:])
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errors.Wrap(err, "kubectl config view server")
	}
	c.srv = strings.TrimSpace(string(output))
	p.Logf("Server: %s", c.srv)

	c.Setup(p.Supervisor(), "cluster", c.check, func(p *supervisor.Process) error { c.SetDone(); return nil })
	return c, nil
}

// getClusterPreviewHostname returns the hostname of the first Host resource it
// finds that has Preview URLs enabled with a supported URL type.
func (c *k8sCluster) getClusterPreviewHostname(p *supervisor.Process) (string, error) {
	p.Log("Looking for a Host with Preview URLs enabled")

	// kubectl get hosts, in all namespaces or in this namespace
	outBytes, err := func() ([]byte, error) {
		clusterCmd := c.getKubectlCmdNoNamespace(p, "get", "host", "-o", "yaml", "--all-namespaces")
		if outBytes, err := clusterCmd.CombinedOutput(); err == nil {
			return outBytes, nil
		}

		nsCmd := c.getKubectlCmd(p, "get", "host", "-o", "yaml")
		if outBytes, err := nsCmd.CombinedOutput(); err == nil {
			return outBytes, nil
		} else {
			return nil, err
		}
	}()
	if err != nil {
		return "", err
	}

	// Parse the output
	hostLists, err := k8s.ParseResources("get hosts", string(outBytes))
	if err != nil {
		return "", err
	}
	if len(hostLists) != 1 {
		return "", errors.Errorf("weird result with length %d", len(hostLists))
	}

	// Grab the "items" slice, as the result should be a list of Host resources
	hostItems := k8s.Map(hostLists[0]).GetMaps("items")
	p.Logf("Found %d Host resources", len(hostItems))

	// Loop over Hosts looking for a Preview URL hostname
	for _, hostItem := range hostItems {
		host := k8s.Resource(hostItem)
		logEntry := fmt.Sprintf("- Host %s / %s: %%s", host.Namespace(), host.Name())

		previewUrlSpec := host.Spec().GetMap("previewUrl")
		if len(previewUrlSpec) == 0 {
			p.Logf(logEntry, "no preview URL config")
			continue
		}

		if enabled, ok := previewUrlSpec["enabled"].(bool); !ok || !enabled {
			p.Logf(logEntry, "preview URL not enabled")
			continue
		}

		// missing type, default is "Path" --> success
		// type is present, set to "Path" --> success
		// otherwise --> failure
		if pType, ok := previewUrlSpec["type"].(string); ok && pType != "Path" {
			p.Logf(logEntry+": %#v", "unsupported preview URL type", previewUrlSpec["type"])
			continue
		}

		var hostname string
		if hostname = host.Spec().GetString("hostname"); hostname == "" {
			p.Logf(logEntry, "empty hostname???")
			continue
		}

		p.Logf(logEntry+": %q", "SUCCESS! Hostname is", hostname)
		return hostname, nil
	}

	p.Logf("No appropriate Host resource found.")
	return "", nil
}