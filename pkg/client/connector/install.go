package connector

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blang/semver"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	errors2 "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/datawire/ambassador/pkg/kates"
	"github.com/datawire/dlib/dlog"
	"github.com/datawire/dlib/dtime"
	"github.com/telepresenceio/telepresence/rpc/v2/manager"
	"github.com/telepresenceio/telepresence/v2/pkg/client"
)

type installer struct {
	*k8sCluster
}

func newTrafficManagerInstaller(kc *k8sCluster) (*installer, error) {
	return &installer{k8sCluster: kc}, nil
}

const ManagerPortSSH = 8022
const ManagerPortHTTP = 8081
const managerAppName = "traffic-manager"
const telName = "manager"
const domainPrefix = "telepresence.getambassador.io/"
const annTelepresenceActions = domainPrefix + "actions"
const agentContainerName = "traffic-agent"

// this is modified in tests
var managerNamespace = func() string {
	if ns := os.Getenv("TELEPRESENCE_MANAGER_NAMESPACE"); ns != "" {
		return ns
	}
	return "ambassador"
}()

var labelMap = map[string]string{
	"app":          managerAppName,
	"telepresence": telName,
}

var (
	managerImage       string
	resolveManagerName = sync.Once{}
)

func managerImageName(env client.Env) string {
	resolveManagerName.Do(func() {
		managerImage = fmt.Sprintf("%s/tel2:%s", env.Registry, strings.TrimPrefix(client.Version(), "v"))
	})
	return managerImage
}

func (ki *installer) createManagerSvc(c context.Context) (*kates.Service, error) {
	svc := &kates.Service{
		TypeMeta: kates.TypeMeta{
			Kind: "Service",
		},
		ObjectMeta: kates.ObjectMeta{
			Namespace: managerNamespace,
			Name:      managerAppName},
		Spec: kates.ServiceSpec{
			Type:      "ClusterIP",
			ClusterIP: "None",
			Selector:  labelMap,
			Ports: []kates.ServicePort{
				{
					Name: "sshd",
					Port: ManagerPortSSH,
					TargetPort: kates.IntOrString{
						Type:   intstr.String,
						StrVal: "sshd",
					},
				},
				{
					Name: "api",
					Port: ManagerPortHTTP,
					TargetPort: kates.IntOrString{
						Type:   intstr.String,
						StrVal: "api",
					},
				},
			},
		},
	}

	// Ensure that the managerNamespace exists
	if !ki.namespaceExists(managerNamespace) {
		ns := &kates.Namespace{
			TypeMeta:   kates.TypeMeta{Kind: "Namespace"},
			ObjectMeta: kates.ObjectMeta{Name: managerNamespace},
		}
		dlog.Infof(c, "Creating namespace %q", managerNamespace)
		if err := ki.client.Create(c, ns, ns); err != nil {
			// trap race condition. If it's there, then all is good.
			if !errors2.IsAlreadyExists(err) {
				return nil, err
			}
		}
	}

	dlog.Infof(c, "Installing traffic-manager service in namespace %s", managerNamespace)
	if err := ki.client.Create(c, svc, svc); err != nil {
		return nil, err
	}
	return svc, nil
}

func (ki *installer) createManagerDeployment(c context.Context, env client.Env) error {
	dep := ki.managerDeployment(env)
	dlog.Infof(c, "Installing traffic-manager deployment in namespace %s. Image: %s", managerNamespace, managerImageName(env))
	return ki.client.Create(c, dep, dep)
}

// removeManager will remove the agent from all deployments listed in the given agents slice. Unless agentsOnly is true,
// it will also remove the traffic-manager service and deployment.
func (ki *installer) removeManagerAndAgents(c context.Context, agentsOnly bool, agents []*manager.AgentInfo) error {
	// Removes the manager and all agents from the cluster
	var errs []error
	var errsLock sync.Mutex
	addError := func(e error) {
		errsLock.Lock()
		errs = append(errs, e)
		errsLock.Unlock()
	}

	// Remove the agent from all deployments
	wg := sync.WaitGroup{}
	wg.Add(len(agents))
	for _, ai := range agents {
		ai := ai // pin it
		go func() {
			defer wg.Done()
			kind, err := ki.findObjectKind(c, ai.Namespace, ai.Name)
			if err != nil {
				addError(err)
				return
			}
			var agent kates.Object
			switch kind {
			case "ReplicaSet":
				agent, err = ki.findReplicaSet(c, ai.Namespace, ai.Name)
				if err != nil {
					if !errors2.IsNotFound(err) {
						addError(err)
					}
					return
				}
			case "Deployment":
				agent, err = ki.findDeployment(c, ai.Namespace, ai.Name)
				if err != nil {
					if !errors2.IsNotFound(err) {
						addError(err)
					}
					return
				}
			default:
				addError(fmt.Errorf("Agent associated with unknown workload kind, can't be removed: %s", ai.Name))
				return
			}
			if err = ki.undoObjectMods(c, agent); err != nil {
				addError(err)
				return
			}
			if err = ki.waitForApply(c, ai.Namespace, ai.Name, agent); err != nil {
				addError(err)
			}
		}()
	}
	// wait for all agents to be removed
	wg.Wait()

	if !agentsOnly && len(errs) == 0 {
		// agent removal succeeded. Remove the manager service and deployment
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := ki.removeManagerService(c); err != nil {
				addError(err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := ki.removeManagerDeployment(c); err != nil {
				addError(err)
			}
		}()
		wg.Wait()
	}

	switch len(errs) {
	case 0:
	case 1:
		return errs[0]
	default:
		bld := bytes.NewBufferString("multiple errors:")
		for _, err := range errs {
			bld.WriteString("\n  ")
			bld.WriteString(err.Error())
		}
		return errors.New(bld.String())
	}
	return nil
}

func (ki *installer) removeManagerService(c context.Context) error {
	svc := &kates.Service{
		TypeMeta: kates.TypeMeta{
			Kind: "Service",
		},
		ObjectMeta: kates.ObjectMeta{
			Namespace: managerNamespace,
			Name:      managerAppName}}
	dlog.Infof(c, "Deleting traffic-manager service from namespace %s", managerNamespace)
	return ki.client.Delete(c, svc, svc)
}

func (ki *installer) removeManagerDeployment(c context.Context) error {
	dep := &kates.Deployment{
		TypeMeta: kates.TypeMeta{
			Kind: "Deployment",
		},
		ObjectMeta: kates.ObjectMeta{
			Namespace: managerNamespace,
			Name:      managerAppName,
		}}
	dlog.Infof(c, "Deleting traffic-manager deployment from namespace %s", managerNamespace)
	return ki.client.Delete(c, dep, dep)
}

func (ki *installer) updateDeployment(c context.Context, env client.Env, currentDep *kates.Deployment) (*kates.Deployment, error) {
	dep := ki.managerDeployment(env)
	dep.ResourceVersion = currentDep.ResourceVersion
	dlog.Infof(c, "Updating traffic-manager deployment in namespace %s. Image: %s", managerNamespace, managerImageName(env))
	err := ki.client.Update(c, dep, dep)
	if err != nil {
		return nil, err
	}
	return dep, err
}

func svcPortByName(svc *kates.Service, name string) []*kates.ServicePort {
	svcPorts := make([]*kates.ServicePort, 0)
	ports := svc.Spec.Ports
	for i := range ports {
		port := &ports[i]
		if name == "" || name == port.Name {
			svcPorts = append(svcPorts, port)
		}
	}
	return svcPorts
}

func (ki *installer) findMatchingServices(portName, svcName string, labels map[string]string) []*kates.Service {
	matching := make([]*kates.Service, 0)

	ki.accLock.Lock()
	for _, watch := range ki.watchers {
	nextSvc:
		for _, svc := range watch.Services {
			selector := svc.Spec.Selector
			if len(selector) == 0 {
				continue nextSvc
			}
			// Only check if the service names are equal when supplied by user
			if svcName != "" && svc.Name != svcName {
				continue nextSvc
			}
			for k, v := range selector {
				if labels[k] != v {
					continue nextSvc
				}
			}
			if len(svcPortByName(svc, portName)) > 0 {
				matching = append(matching, svc)
			}
		}
	}
	ki.accLock.Unlock()
	return matching
}

func findMatchingPort(obj kates.Object, portName string, svcs []*kates.Service) (
	service *kates.Service,
	sPort *kates.ServicePort,
	cn *kates.Container,
	cPortIndex int,
	err error,
) {
	podTemplate, objType, err := GetPodTemplateFromObject(obj)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	// Sort slice of services so that the ones in the same namespace get prioritized.
	sort.Slice(svcs, func(i, j int) bool {
		a := svcs[i]
		b := svcs[j]
		objNamespace := obj.GetNamespace()
		if a.Namespace != b.Namespace {
			if a.Namespace == objNamespace {
				return true
			}
			if b.Namespace == objNamespace {
				return false
			}
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	cns := podTemplate.Spec.Containers
	for _, svc := range svcs {
		// For now, we only support intercepting one port on a given service.
		ports := svcPortByName(svc, portName)
		if len(ports) == 0 {
			// this may happen when portName is specified but none of the ports match
			continue
		}
		if len(ports) > 1 {
			return nil, nil, nil, 0, fmt.Errorf(`
found matching service with multiple ports for %s %s.%s. Please specify the
service port you want to intercept like so --port local:svcPortName`,
				objType, obj.GetName(), obj.GetNamespace())
		}
		port := ports[0]
		var msp *corev1.ServicePort
		var ccn *corev1.Container
		var cpi int

		if port.TargetPort.Type == intstr.String {
			portName := port.TargetPort.StrVal
			for ci := 0; ci < len(cns) && ccn == nil; ci++ {
				cn := &cns[ci]
				for pi := range cn.Ports {
					if cn.Ports[pi].Name == portName {
						msp = port
						ccn = cn
						cpi = pi
						break
					}
				}
			}
		} else {
			portNum := port.TargetPort.IntVal
			// Here we are using cpi <=0 instead of ccn == nil because if a
			// container has no ports, we want to use it but we don't want
			// to break out of the loop looking at containers in case there
			// is a better fit.  Currently, that is a container where the
			// ContainerPort matches the targetPort in the service.
			for ci := 0; ci < len(cns) && cpi <= 0; ci++ {
				cn := &cns[ci]
				if len(cn.Ports) == 0 {
					msp = port
					ccn = cn
					cpi = -1
				}
				for pi := range cn.Ports {
					if cn.Ports[pi].ContainerPort == portNum {
						msp = port
						ccn = cn
						cpi = pi
						break
					}
				}
			}
		}

		switch {
		case msp == nil:
			continue
		case sPort == nil:
			service = svc
			sPort = msp
			cPortIndex = cpi
			cn = ccn
		case sPort.TargetPort == msp.TargetPort:
			// Keep the chosen one
		case sPort.TargetPort.Type == intstr.String && msp.TargetPort.Type == intstr.Int:
			// Keep the chosen one
		case sPort.TargetPort.Type == intstr.Int && msp.TargetPort.Type == intstr.String:
			// Prefer targetPort in string format
			service = svc
			sPort = msp
			cPortIndex = cpi
			cn = ccn
		default:
			// Conflict
			return nil, nil, nil, 0, fmt.Errorf(
				"found services with conflicting port mappings to %s %s.%s. Please use --service to specify", objType, obj.GetName(), obj.GetNamespace())
		}
	}

	if sPort == nil {
		return nil, nil, nil, 0, fmt.Errorf("found no services with a port that matches a container in %s %s.%s", objType, obj.GetName(), obj.GetNamespace())
	}
	return service, sPort, cn, cPortIndex, nil
}

// Finds the Referenced Service in an objects' annotations
func (ki *installer) getSvcFromObjAnnotation(c context.Context, obj kates.Object) (*kates.Service, error) {
	var actions workloadActions
	annotationsFound, err := getAnnotation(obj, &actions)
	if err != nil {
		return nil, err
	}
	namespace := obj.GetNamespace()
	if !annotationsFound {
		return nil, fmt.Errorf("No annotations found on deployment: %s.%s", obj.GetName(), namespace)
	}
	svcName := actions.ReferencedService
	if svcName == "" {
		return nil, fmt.Errorf("No ReferencedService found on deployment: %s.%s", obj.GetName(), namespace)
	}

	svc := ki.findSvc(namespace, svcName)
	if svc == nil {
		return nil, fmt.Errorf("Deployment %s.%s referenced unfound service: %s", obj.GetName(), namespace, svcName)
	}
	return svc, nil
}

// Determines if the service associated with a pre-existing intercept exists or if
// the port to-be-intercepted has changed. It raises an error if either of these
// cases exist since to go forward with an intercept would require changing the
// configuration of the agent.
func checkSvcSame(c context.Context, obj kates.Object, svcName, portName string) error {
	var actions workloadActions
	annotationsFound, err := getAnnotation(obj, &actions)
	if err != nil {
		return err
	}
	if annotationsFound {
		// If the Service in the annotation doesn't match the svcName passed in
		// then the service to be used with the intercept has changed
		curSvc := actions.ReferencedService
		if svcName != "" && curSvc != svcName {
			return fmt.Errorf("Service for %s changed from %s -> %s", obj.GetName(), curSvc, svcName)
		}

		// If the portName in the annotation doesn't match the portName passed in
		// then the ports have changed.
		curSvcPort := actions.ReferencedServicePortName
		if curSvcPort != portName {
			return fmt.Errorf("Port for %s changed from %s -> %s", obj.GetName(), curSvcPort, portName)
		}
	}
	return nil
}

var agentNotFound = errors.New("no such agent")

// This does a lot of things but at a high level it ensures that the traffic agent
// is installed alongside the proper workload. In doing that, it also ensures that
// the workload is referenced by a service. Lastly, it returns the service UID
// associated with the workload since this is where that correlation is made.
func (ki *installer) ensureAgent(c context.Context, namespace, name, svcName, portName, agentImageName string) (string, string, error) {
	kind, err := ki.findObjectKind(c, namespace, name)
	if err != nil {
		return "", "", err
	}
	var obj kates.Object
	switch kind {
	case "ReplicaSet":
		obj, err = ki.findReplicaSet(c, namespace, name)
		if err != nil {
			return "", "", err
		}
	case "Deployment":
		obj, err = ki.findDeployment(c, namespace, name)
		if err != nil {
			return "", "", err
		}
	default:
		return "", "", fmt.Errorf("Cannot ensure agent on unsupported workload: %s", kind)
	}

	podTemplate, _, err := GetPodTemplateFromObject(obj)
	if err != nil {
		return "", "", err
	}
	var agentContainer *kates.Container
	for i := range podTemplate.Spec.Containers {
		container := &podTemplate.Spec.Containers[i]
		if container.Name == agentContainerName {
			agentContainer = container
			break
		}
	}

	if err := checkSvcSame(c, obj, svcName, portName); err != nil {
		msg := fmt.Sprintf(
			`%s already being used for intercept with a different service
configuration. To intercept this with your new configuration, please use
telepresence uninstall --agent %s This will cancel any intercepts that
already exist for this service`, kind, obj.GetName())
		return "", "", errors.Wrap(err, msg)
	}
	var svc *kates.Service

	switch {
	case agentContainer == nil:
		dlog.Infof(c, "no agent found for %s %s.%s", kind, name, namespace)
		dlog.Infof(c, "Using port name %q", portName)
		matchingSvcs := ki.findMatchingServices(portName, svcName, podTemplate.Labels)

		switch numSvcs := len(matchingSvcs); {
		case numSvcs == 0:
			errMsg := fmt.Sprintf("Found no services with a selector matching labels %v", podTemplate.Labels)
			if portName != "" {
				errMsg += fmt.Sprintf(" and a port named %s", portName)
			}
			return "", "", errors.New(errMsg)
		case numSvcs > 1:
			svcNames := make([]string, 0, numSvcs)
			for _, svc := range matchingSvcs {
				svcNames = append(svcNames, svc.Name)
			}

			errMsg := fmt.Sprintf("Found multiple services with a selector matching labels %v: %s",
				podTemplate.Labels, strings.Join(svcNames, ","))
			if portName != "" {
				errMsg += fmt.Sprintf(" and a port named %s", portName)
			}
			return "", "", errors.New(errMsg)
		default:
		}

		var err error
		obj, svc, err = addAgentToDeployment(c, portName, agentImageName, obj, matchingSvcs)
		if err != nil {
			return "", "", err
		}
	case agentContainer.Image != agentImageName:
		var actions workloadActions
		ok, err := getAnnotation(obj, &actions)
		if err != nil {
			return "", "", err
		} else if !ok {
			// This can only happen if someone manually tampered with the annTelepresenceActions annotation
			return "", "", fmt.Errorf("expected %q annotation not found in %s.%s", annTelepresenceActions, name, namespace)
		}

		dlog.Debugf(c, "Updating agent for %s %s.%s", kind, name, namespace)
		aaa := &workloadActions{
			Version:         actions.Version,
			AddTrafficAgent: actions.AddTrafficAgent,
		}
		explainUndo(c, aaa, obj)
		aaa.AddTrafficAgent.ImageName = agentImageName
		agentContainer.Image = agentImageName
		explainDo(c, aaa, obj)
	default:
		dlog.Debugf(c, "%s %s.%s already has an installed and up-to-date agent", kind, name, namespace)
	}

	if err := ki.client.Update(c, obj, obj); err != nil {
		return "", "", err
	}
	if svc != nil {
		if err := ki.client.Update(c, svc, svc); err != nil {
			return "", "", err
		}
	} else {
		// If the service is still nil, that's because an agent already exists that we can reuse.
		// So we get the service from the deployments annotation so that we can extract the UID.
		svc, err = ki.getSvcFromObjAnnotation(c, obj)
		if err != nil {
			return "", "", err
		}
	}

	if err := ki.waitForApply(c, namespace, name, obj); err != nil {
		return "", "", err
	}
	return string(svc.GetUID()), kind, nil
}

func (ki *installer) waitForApply(c context.Context, namespace, name string, obj kates.Object) error {
	tos := &client.GetConfig(c).Timeouts
	c, cancel := context.WithTimeout(c, tos.Apply)
	defer cancel()

	origGeneration := int64(0)
	if obj != nil {
		origGeneration = obj.GetGeneration()
	}
	kind, err := ki.findObjectKind(c, namespace, name)
	if err != nil {
		return err
	}
	switch kind {
	case "ReplicaSet":
		err := ki.refreshReplicaSet(c, name, namespace)
		if err != nil {
			return err
		}
		for {
			dtime.SleepWithContext(c, time.Second)
			if err := client.CheckTimeout(c, &tos.Apply, nil); err != nil {
				return err
			}

			rs, err := ki.findReplicaSet(c, namespace, name)
			if err != nil {
				return client.CheckTimeout(c, &tos.Apply, err)
			}

			deployed := rs.ObjectMeta.Generation >= origGeneration &&
				rs.Status.ObservedGeneration == rs.ObjectMeta.Generation &&
				(rs.Spec.Replicas == nil || rs.Status.Replicas >= *rs.Spec.Replicas) &&
				rs.Status.FullyLabeledReplicas == rs.Status.Replicas &&
				rs.Status.AvailableReplicas == rs.Status.Replicas

			if deployed {
				dlog.Debugf(c, "Replica Set %s.%s successfully applied", name, namespace)
				return nil
			}
		}
	case "Deployment":
		for {
			dtime.SleepWithContext(c, time.Second)
			if err := client.CheckTimeout(c, &tos.Apply, nil); err != nil {
				return err
			}

			dep, err := ki.findDeployment(c, namespace, name)
			if err != nil {
				return client.CheckTimeout(c, &tos.Apply, err)
			}

			deployed := dep.ObjectMeta.Generation >= origGeneration &&
				dep.Status.ObservedGeneration == dep.ObjectMeta.Generation &&
				(dep.Spec.Replicas == nil || dep.Status.UpdatedReplicas >= *dep.Spec.Replicas) &&
				dep.Status.UpdatedReplicas == dep.Status.Replicas &&
				dep.Status.AvailableReplicas == dep.Status.Replicas

			if deployed {
				dlog.Debugf(c, "deployment %s.%s successfully applied", name, namespace)
				return nil
			}
		}

	default:
		return fmt.Errorf("Can't wait for apply of unknown workload type: %s", kind)
	}
}

// refreshReplicaSet finds pods owned by a given ReplicaSet and deletes them.
// We need this because updating a Replica Set does *not* generate new
// pods if the desired amount already exists.
func (ki *installer) refreshReplicaSet(c context.Context, name, namespace string) error {
	rs, err := ki.findReplicaSet(c, namespace, name)
	if err != nil {
		return err
	}

	podNames, err := ki.podNames(c, namespace)
	if err != nil {
		return err
	}

	for _, podName := range podNames {
		podInfo, err := ki.findPod(c, namespace, podName)
		if err != nil {
			return err
		}

		for _, ownerRef := range podInfo.OwnerReferences {
			if ownerRef.UID == rs.UID {
				dlog.Infof(c, "Deleting pod %s owned by rs %s", podInfo.Name, rs.Name)
				pod := &kates.Pod{
					TypeMeta: kates.TypeMeta{
						Kind: "Pod",
					},
					ObjectMeta: kates.ObjectMeta{
						Namespace: podInfo.Namespace,
						Name:      podInfo.Name,
					},
				}
				if err := ki.client.Delete(c, pod, pod); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func getAnnotation(obj kates.Object, data completeAction) (bool, error) {
	ann := obj.GetAnnotations()
	if ann == nil {
		return false, nil
	}
	ajs, ok := ann[annTelepresenceActions]
	if !ok {
		return false, nil
	}
	if err := data.UnmarshalAnnotation(ajs); err != nil {
		return false, err
	}

	annV, err := semver.Parse(data.TelVersion())
	if err != nil {
		return false, fmt.Errorf("unable to parse semantic version in annotation %s of %s %s", annTelepresenceActions,
			obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
	}
	ourV := client.Semver()

	// Compare major and minor versions. 100% backward compatibility is assumed and greater patch versions are allowed
	if ourV.Major < annV.Major || ourV.Major == annV.Major && ourV.Minor < annV.Minor {
		return false, fmt.Errorf("the version %v found in annotation %s of %s %s is more recent than version %v of this binary",
			annV, annTelepresenceActions, obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), ourV)
	}
	return true, nil
}

func (ki *installer) undoObjectMods(c context.Context, obj kates.Object) error {
	referencedService, err := undoObjectMods(c, obj)
	if err != nil {
		return err
	}
	if svc := ki.findSvc(obj.GetNamespace(), referencedService); svc != nil {
		if err = ki.undoServiceMods(c, svc); err != nil {
			return err
		}
	}
	return ki.client.Update(c, obj, obj)
}

func undoObjectMods(c context.Context, obj kates.Object) (string, error) {
	var actions workloadActions
	ok, err := getAnnotation(obj, &actions)
	if !ok {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("deployment %s.%s has no agent installed", obj.GetName(), obj.GetNamespace())
	}

	if err = actions.Undo(obj); err != nil {
		return "", err
	}
	annotations := obj.GetAnnotations()
	delete(annotations, annTelepresenceActions)
	if len(annotations) == 0 {
		obj.SetAnnotations(nil)
	}
	explainUndo(c, &actions, obj)
	return actions.ReferencedService, nil
}

func (ki *installer) undoServiceMods(c context.Context, svc *kates.Service) error {
	if err := undoServiceMods(c, svc); err != nil {
		return err
	}
	return ki.client.Update(c, svc, svc)
}

func undoServiceMods(c context.Context, svc *kates.Service) error {
	var actions svcActions
	ok, err := getAnnotation(svc, &actions)
	if !ok {
		return err
	}
	if err = actions.Undo(svc); err != nil {
		return err
	}
	delete(svc.Annotations, annTelepresenceActions)
	if len(svc.Annotations) == 0 {
		svc.Annotations = nil
	}
	explainUndo(c, &actions, svc)
	return nil
}

func addAgentToDeployment(
	c context.Context,
	portName string,
	agentImageName string,
	object kates.Object, matchingServices []*kates.Service,
) (
	kates.Object,
	*kates.Service,
	error,
) {
	_, kind, err := GetPodTemplateFromObject(object)
	if err != nil {
		return nil, nil, err
	}
	service, servicePort, container, containerPortIndex, err := findMatchingPort(object, portName, matchingServices)
	if err != nil {
		return nil, nil, err
	}
	dlog.Debugf(c, "using service %q port %q when intercepting %s %q",
		service.Name,
		func() string {
			if servicePort.Name != "" {
				return servicePort.Name
			}
			return strconv.Itoa(int(servicePort.Port))
		}(),
		kind,
		object.GetName())

	version := client.Semver().String()

	// Try to detect the container port we'll be taking over.
	var containerPort struct {
		Name     string // If the existing container port doesn't have a name, we'll make one up.
		Number   uint16
		Protocol corev1.Protocol
	}

	// Start by filling from the servicePort; if these are the zero values, that's OK.
	svcHasTargetPort := true
	if servicePort.TargetPort.Type == intstr.Int {
		if servicePort.TargetPort.IntVal == 0 {
			containerPort.Number = uint16(servicePort.Port)
			svcHasTargetPort = false
		} else {
			containerPort.Number = uint16(servicePort.TargetPort.IntVal)
		}
	} else {
		containerPort.Name = servicePort.TargetPort.StrVal
	}
	containerPort.Protocol = servicePort.Protocol

	// Now fill from the Deployment's containerPort.
	usedContainerName := false
	if containerPortIndex >= 0 {
		if containerPort.Name == "" {
			containerPort.Name = container.Ports[containerPortIndex].Name
			if containerPort.Name != "" {
				usedContainerName = true
			}
		}
		if containerPort.Number == 0 {
			containerPort.Number = uint16(container.Ports[containerPortIndex].ContainerPort)
		}
		if containerPort.Protocol == "" {
			containerPort.Protocol = container.Ports[containerPortIndex].Protocol
		}
	}
	if containerPort.Number == 0 {
		return nil, nil, fmt.Errorf("unable to add agent to %s %s.%s. The container port cannot be determined", kind, object.GetName(), object.GetNamespace())
	}
	if containerPort.Name == "" {
		containerPort.Name = fmt.Sprintf("tel2px-%d", containerPort.Number)
	}

	// Figure what modifications we need to make.
	deploymentMod := &workloadActions{
		Version:                   version,
		ReferencedService:         service.Name,
		ReferencedServicePortName: portName,
		AddTrafficAgent: &addTrafficAgentAction{
			containerName:       container.Name,
			ContainerPortName:   containerPort.Name,
			ContainerPortProto:  containerPort.Protocol,
			ContainerPortNumber: containerPort.Number,
			ImageName:           agentImageName,
		},
	}
	// Depending on whether the Service refers to the port by name or by number, we either need
	// to patch the names in the deployment, or the number in the service.
	var serviceMod *svcActions
	if servicePort.TargetPort.Type == intstr.Int {
		// Change the port number that the Service refers to.
		serviceMod = &svcActions{Version: version}
		if svcHasTargetPort {
			serviceMod.MakePortSymbolic = &makePortSymbolicAction{
				PortName:     servicePort.Name,
				TargetPort:   containerPort.Number,
				SymbolicName: containerPort.Name,
			}
		} else {
			serviceMod.AddSymbolicPort = &addSymbolicPortAction{
				makePortSymbolicAction{
					PortName:     servicePort.Name,
					TargetPort:   containerPort.Number,
					SymbolicName: containerPort.Name,
				},
			}
		}
		// Since we are updating the service to use the containerPort.Name
		// if that value came from the container, then we need to hide it
		// since the service is using the targetPort's int.
		if usedContainerName {
			deploymentMod.HideContainerPort = &hideContainerPortAction{
				ContainerName: container.Name,
				PortName:      containerPort.Name,
			}
		}
	} else {
		// Hijack the port name in the Deployment.
		deploymentMod.HideContainerPort = &hideContainerPortAction{
			ContainerName: container.Name,
			PortName:      containerPort.Name,
		}
	}

	// Apply the actions on the Deployment.
	if err = deploymentMod.Do(object); err != nil {
		return nil, nil, err
	}
	annotations := object.GetAnnotations()
	if object.GetAnnotations() == nil {
		annotations = make(map[string]string)
	}
	annotations[annTelepresenceActions], err = deploymentMod.MarshalAnnotation()
	if err != nil {
		return nil, nil, err
	}
	object.SetAnnotations(annotations)
	explainDo(c, deploymentMod, object)

	// Apply the actions on the Service.
	if serviceMod != nil {
		if err = serviceMod.Do(service); err != nil {
			return nil, nil, err
		}
		if service.Annotations == nil {
			service.Annotations = make(map[string]string)
		}
		service.Annotations[annTelepresenceActions], err = serviceMod.MarshalAnnotation()
		if err != nil {
			return nil, nil, err
		}
		explainDo(c, serviceMod, service)
	} else {
		service = nil
	}

	return object, service, nil
}

func (ki *installer) managerDeployment(env client.Env) *kates.Deployment {
	replicas := int32(1)

	var containerEnv []corev1.EnvVar

	containerEnv = append(containerEnv, corev1.EnvVar{Name: "LOG_LEVEL", Value: "debug"})
	if env.SystemAHost != "" {
		containerEnv = append(containerEnv, corev1.EnvVar{Name: "SYSTEMA_HOST", Value: env.SystemAHost})
	}
	if env.SystemAPort != "" {
		containerEnv = append(containerEnv, corev1.EnvVar{Name: "SYSTEMA_PORT", Value: env.SystemAPort})
	}

	return &kates.Deployment{
		TypeMeta: kates.TypeMeta{
			Kind: "Deployment",
		},
		ObjectMeta: kates.ObjectMeta{
			Namespace: managerNamespace,
			Name:      managerAppName,
			Labels:    labelMap,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labelMap,
			},
			Template: kates.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labelMap,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  managerAppName,
							Image: managerImageName(env),
							Env:   containerEnv,
							Ports: []corev1.ContainerPort{
								{
									Name:          "sshd",
									ContainerPort: ManagerPortSSH,
								},
								{
									Name:          "api",
									ContainerPort: ManagerPortHTTP,
								},
							}}},
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}
}

func (ki *installer) findManagerSvc(c context.Context) (*kates.Service, error) {
	svc := &kates.Service{
		TypeMeta:   kates.TypeMeta{Kind: "Service"},
		ObjectMeta: kates.ObjectMeta{Name: managerAppName, Namespace: managerNamespace},
	}
	if err := ki.client.Get(c, svc, svc); err != nil {
		return nil, err
	}
	return svc, nil
}

func (ki *installer) ensureManager(c context.Context, env client.Env) error {
	if _, err := ki.findManagerSvc(c); err != nil {
		if errors2.IsNotFound(err) {
			_, err = ki.createManagerSvc(c)
		}
		if err != nil {
			return err
		}
	}

	dep, err := ki.findDeployment(c, managerNamespace, managerAppName)
	if err != nil {
		if errors2.IsNotFound(err) {
			err = ki.createManagerDeployment(c, env)
			if err == nil {
				err = ki.waitForApply(c, managerNamespace, managerAppName, nil)
			}
		}
		return err
	}

	imageName := managerImageName(env)
	cns := dep.Spec.Template.Spec.Containers
	upToDate := false
	for i := range cns {
		cn := &cns[i]
		if cn.Image == imageName {
			upToDate = true
			break
		}
	}
	if upToDate {
		dlog.Infof(c, "%s.%s is up-to-date. Image: %s", managerAppName, managerNamespace, managerImageName(env))
	} else {
		_, err = ki.updateDeployment(c, env, dep)
		if err == nil {
			err = ki.waitForApply(c, managerNamespace, managerAppName, dep)
		}
	}
	return err
}
