package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	core "k8s.io/api/core/v1"
	errors2 "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/telepresenceio/telepresence/rpc/v2/manager"
	"github.com/telepresenceio/telepresence/v2/pkg/install"
	"github.com/telepresenceio/telepresence/v2/pkg/install/agent"
	"github.com/telepresenceio/telepresence/v2/pkg/k8sapi"
	"github.com/telepresenceio/telepresence/v2/pkg/version"
)

func PrepareIntercept(ctx context.Context, cr *manager.CreateInterceptRequest) (*manager.PreparedIntercept, error) {
	interceptError := func(err error) (*manager.PreparedIntercept, error) {
		return &manager.PreparedIntercept{Error: err.Error()}, nil
	}

	spec := cr.InterceptSpec
	wl, err := k8sapi.GetWorkload(ctx, spec.Agent, spec.Namespace, spec.WorkloadKind)
	if err != nil {
		return interceptError(err)
	}
	cmAPI := k8sapi.GetK8sInterface(ctx).CoreV1().ConfigMaps(spec.Namespace)
	cm, err := loadConfigMap(ctx, cmAPI, spec.Namespace)
	if err != nil {
		return interceptError(err)
	}
	ac, err := loadAgentConfig(ctx, cmAPI, cm, wl)
	if err != nil {
		return interceptError(err)
	}
	_, ics, err := findIntercepts(ac, spec)
	if err != nil {
		return interceptError(err)
	}
	svcs := make([]*manager.PreparedIntercept_Service, len(ics))
	for i, ic := range ics {
		svcs[i] = &manager.PreparedIntercept_Service{
			ServiceUid:      string(ic.ServiceUID),
			ServiceName:     ic.ServiceName,
			ServicePortName: ic.ServicePortName,
			ServicePort:     int32(ic.ServicePort),
		}
	}
	return &manager.PreparedIntercept{
		Namespace:    spec.Namespace,
		Services:     svcs,
		AgentImage:   ac.AgentImage,
		WorkloadKind: ac.WorkloadKind,
	}, nil
}

func loadConfigMap(ctx context.Context, cmAPI v1.ConfigMapInterface, namespace string) (*core.ConfigMap, error) {
	cm, err := cmAPI.Get(ctx, agent.ConfigMap, meta.GetOptions{})
	if err == nil {
		return cm, nil
	}
	if !errors2.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get ConfigMap %s.%s: %w", agent.ConfigMap, namespace, err)
	}
	cm, err = cmAPI.Create(ctx, &core.ConfigMap{
		TypeMeta: meta.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: meta.ObjectMeta{
			Name:      agent.ConfigMap,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       agent.ConfigMap,
				"app.kubernetes.io/created-by": "traffic-manager",
				"app.kubernetes.io/version":    strings.TrimPrefix(version.Version, "v"),
			},
		},
	}, meta.CreateOptions{})
	if err != nil {
		err = fmt.Errorf("failed to create ConfigMap %s.%s: %w", agent.ConfigMap, namespace, err)
	}
	return cm, err
}

func loadAgentConfig(ctx context.Context, cmAPI v1.ConfigMapInterface, cm *core.ConfigMap, wl k8sapi.Workload) (*agent.Config, error) {
	manuallyManaged, enabled, err := checkInterceptAnnotations(wl)
	if err != nil {
		return nil, err
	}
	if !(manuallyManaged || enabled) {
		return nil, fmt.Errorf("%s %s.%s is not interceptable", wl.GetKind(), wl.GetName(), wl.GetNamespace())
	}

	var ac *agent.Config
	if y, ok := cm.Data[wl.GetName()]; ok {
		if ac, err = unmarshalConfigMapEntry(y, wl.GetName(), wl.GetNamespace()); err != nil {
			return nil, err
		}
		if ac.Create {
			// This may happen if someone else is doing the initial intercept at the exact (well, more or less) same time
			if ac, err = waitForConfigMapUpdate(ctx, cmAPI, wl.GetName(), wl.GetNamespace()); err != nil {
				return nil, err
			}
		}
	} else {
		if manuallyManaged {
			return nil, fmt.Errorf(
				"annotation %s.%s/%s=true but workload has no corresponding entry in the %s ConfigMap",
				wl.GetName(), wl.GetNamespace(), install.ManualInjectAnnotation, agent.ConfigMap)
		}
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[wl.GetName()] = fmt.Sprintf("create: true\nworkloadKind: %s\nworkloadName: %s\nnamespace: %s",
			wl.GetKind(), wl.GetName(), wl.GetNamespace())
		if _, err := cmAPI.Update(ctx, cm, meta.UpdateOptions{}); err != nil {
			return nil, fmt.Errorf("failed update entry for %s in ConfigMap %s.%s: %w", wl.GetName(), agent.ConfigMap, wl.GetNamespace(), err)
		}
		if ac, err = waitForConfigMapUpdate(ctx, cmAPI, wl.GetName(), wl.GetNamespace()); err != nil {
			return nil, err
		}
	}
	return ac, nil
}

func checkInterceptAnnotations(wl k8sapi.Workload) (bool, bool, error) {
	pod := wl.GetPodTemplate()
	a := pod.Annotations
	if a == nil {
		return false, true, nil
	}

	webhookEnabled := true
	manuallyManaged := a[install.ManualInjectAnnotation] == "true"
	ia := a[install.InjectAnnotation]
	switch ia {
	case "":
		webhookEnabled = !manuallyManaged
	case "enabled":
		if manuallyManaged {
			return false, false, fmt.Errorf(
				"annotation %s.%s/%s=enabled cannot be combined with %s=true",
				wl.GetName(), wl.GetNamespace(), install.InjectAnnotation, install.ManualInjectAnnotation)
		}
	case "false", "disabled":
		webhookEnabled = false
	default:
		return false, false, fmt.Errorf(
			"%s is not a valid value for the %s.%s/%s annotation",
			ia, wl.GetName(), wl.GetNamespace(), install.ManualInjectAnnotation)
	}

	if !manuallyManaged {
		return false, webhookEnabled, nil
	}
	cns := pod.Spec.Containers
	var an *core.Container
	for i := range cns {
		cn := &cns[i]
		if cn.Name == agent.ContainerName {
			an = cn
			break
		}
	}
	if an == nil {
		return false, false, fmt.Errorf(
			"annotation %s.%s/%s=true but pod has no traffic-agent container",
			wl.GetName(), wl.GetNamespace(), install.ManualInjectAnnotation)
	}
	return false, true, nil
}

// Wait for the cluster's mutating webhook injector to do its magic. It will update the
// configMap once it's done.
func waitForConfigMapUpdate(ctx context.Context, cmAPI v1.ConfigMapInterface, agentName, namespace string) (*agent.Config, error) {
	wi, err := cmAPI.Watch(ctx, meta.SingleObject(meta.ObjectMeta{
		Name:      agent.ConfigMap,
		Namespace: namespace,
	}))
	if err != nil {
		return nil, fmt.Errorf("Watch of ConfigMap  %s failed: %w", agent.ConfigMap, ctx.Err())
	}
	defer wi.Stop()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("Watch of ConfigMap  %s[%s]: %w", agent.ConfigMap, agentName, ctx.Err())
		case ev, ok := <-wi.ResultChan():
			if !ok {
				return nil, fmt.Errorf("Watch of ConfigMap  %s[%s]: channel closed", agent.ConfigMap, agentName)
			}
			if !(ev.Type == watch.Added || ev.Type == watch.Modified) {
				continue
			}
			if m, ok := ev.Object.(*core.ConfigMap); ok {
				if y, ok := m.Data[agentName]; ok {
					conf, ir := unmarshalConfigMapEntry(y, agentName, namespace)
					if ir != nil {
						return nil, ir
					}
					if !conf.Create {
						return conf, nil
					}
				}
			}
		}
	}
}

func unmarshalConfigMapEntry(y string, name, namespace string) (*agent.Config, error) {
	conf := agent.Config{}
	if err := yaml.Unmarshal([]byte(y), &conf); err != nil {
		return nil, fmt.Errorf("failed to parse entry for %s in ConfigMap %s.%s: %w", name, agent.ConfigMap, namespace, err)
	}
	return &conf, nil
}

// findIntercept finds the intercept configurations that matches the given InterceptSpec's ports
// The returned slice will have the same length as, and corresponding positions to, the spec's serviceInterceptIdentifier slice
func findIntercepts(ac *agent.Config, spec *manager.InterceptSpec) (foundCN *agent.Container, foundICs []*agent.Intercept, err error) {
	spis := spec.ServicePortIdentifiers
	foundICs = make([]*agent.Intercept, len(spis))

	notFoundCount := 0
nextSPI:
	for i, spi := range spis {
		for _, cn := range ac.Containers {
			for _, ic := range cn.Intercepts {
				if !(spec.ServiceName == "" || spec.ServiceName == ic.ServiceName) {
					continue
				}
				if agent.IsInterceptFor(spi, ic) {
					foundICs[i] = ic
					if foundCN != nil && foundCN != cn {
						return nil, nil, errors.New("found multiple matching service ports that spans several containers.\n" +
							"Please specify the Services and/or Service ports you want to intercept " +
							"by passing the --service=<svc> and/or --port=<local:svcPortName> flag.")
					}
					foundCN = cn
					continue nextSPI
				}
			}
		}
		notFoundCount++
	}

	// An intercept spec may contain at maximum one intercept that isn't qualified. It will be assigned to the
	// first unused interface in the found container, or first container with intercepts if none is found yet
	if notFoundCount == 1 {
		findUnused := func(cn *agent.Container) bool {
		nextIC:
			for _, ic := range foundCN.Intercepts {
				for _, fis := range foundICs {
					if ic == fis {
						// already taken
						continue nextIC
					}
				}
				for i, fis := range foundICs {
					if fis == nil {
						foundICs[i] = ic
						foundCN = cn
						notFoundCount--
						return true
					}
				}
			}
			return false
		}
		if foundCN != nil {
			// Has to be in the same container
			findUnused(foundCN)
		} else {
			for _, cn := range ac.Containers {
				if findUnused(cn) {
					break
				}
			}
		}
	}

	if notFoundCount > 0 {
		var svcPorts []string
		for _, si := range spec.ServicePortIdentifiers {
			if si != "" {
				svcPorts = append(svcPorts, si)
			}
		}
		switch {
		case spec.ServiceName != "" && len(svcPorts) > 0:
			err = fmt.Errorf("unable to find all intercepts for service %q, ports %v", spec.ServiceName, svcPorts)
		case spec.ServiceName != "":
			err = fmt.Errorf("unable to find intercept for service %q", spec.ServiceName)
		case len(svcPorts) > 0:
			err = fmt.Errorf("unable to find all intercepts for service ports %v", svcPorts)
		default:
			err = errors.New("unable to find intercept")
		}
	}
	return foundCN, foundICs, err
}
