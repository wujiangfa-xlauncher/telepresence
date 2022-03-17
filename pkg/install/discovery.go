package install

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/telepresenceio/telepresence/v2/pkg/k8sapi"
)

// FilterServicePorts iterates through a list of ports in a service and
// only returns the ports that match the given nameOrNumber. All ports will
// be returned if nameOrNumber is equal to the empty string
func FilterServicePorts(svc *core.Service, nameOrNumber string) ([]core.ServicePort, error) {
	ports := svc.Spec.Ports
	if nameOrNumber == "" {
		return ports, nil
	}
	svcPorts := make([]core.ServicePort, 0)
	if number, err := strconv.Atoi(nameOrNumber); err != nil {
		errs := validation.IsValidPortName(nameOrNumber)
		if len(errs) > 0 {
			return nil, fmt.Errorf(strings.Join(errs, "\n"))
		}
		for _, port := range ports {
			if port.Name == nameOrNumber {
				svcPorts = append(svcPorts, port)
			}
		}
	} else {
		for _, port := range ports {
			pn := int32(0)
			if port.TargetPort.Type == intstr.Int {
				pn = port.TargetPort.IntVal
			}
			if pn == 0 {
				pn = port.Port
			}
			if pn == int32(number) {
				svcPorts = append(svcPorts, port)
			}
		}
	}
	return svcPorts, nil
}

// FindContainerMatchingPort finds the container that matches the given ServicePort. The match is
// made using the Name or the ContainerPort field of each port in each container depending on if
// the service port is symbolic or numeric. The first container with a matching port is returned
// along with the index of the container port that matched.
//
// The first container with no ports at all is returned together with a port index of -1, in case
// no port match could be made and the service port is numeric. This enables intercepts of containers
// that indeed do listen a port but lack a matching port description in the manifest, which is what
// you get if you do:
//
//     kubectl create deploy my-deploy --image my-image
//     kubectl expose deploy my-deploy --port 80 --target-port 8080
func FindContainerMatchingPort(port *core.ServicePort, cns []core.Container) (*core.Container, int) {
	if port.TargetPort.Type == intstr.String {
		portName := port.TargetPort.StrVal
		for ci := range cns {
			cn := &cns[ci]
			for pi := range cn.Ports {
				if cn.Ports[pi].Name == portName {
					return cn, pi
				}
			}
		}
	} else {
		portNum := port.TargetPort.IntVal
		if portNum == 0 {
			// The targetPort default is the value of the port field.
			portNum = port.Port
		}
		for ci := range cns {
			cn := &cns[ci]
			for pi := range cn.Ports {
				if cn.Ports[pi].ContainerPort == portNum {
					return cn, pi
				}
			}
		}
		for ci := range cns {
			cn := &cns[ci]
			if len(cn.Ports) == 0 {
				return cn, -1
			}
		}
	}
	return nil, 0
}

func FindMatchingServices(c context.Context, portNameOrNumber, svcName, namespace string, labels map[string]string) ([]*core.Service, error) {
	// TODO: Expensive on large clusters but the problem goes away once we move the installer to the traffic-manager
	si := k8sapi.GetK8sInterface(c).CoreV1().Services(namespace)
	var ss []core.Service
	if svcName != "" {
		s, err := si.Get(c, svcName, meta.GetOptions{})
		if err != nil {
			return nil, err
		}
		ss = []core.Service{*s}
	} else {
		sl, err := si.List(c, meta.ListOptions{})
		if err != nil {
			return nil, err
		}
		ss = sl.Items
	}

	// Returns true if selector is completely included in labels
	labelsMatch := func(selector map[string]string) bool {
		if len(selector) == 0 || len(labels) < len(selector) {
			return false
		}
		for k, v := range selector {
			if labels[k] != v {
				return false
			}
		}
		return true
	}

	var matching []*core.Service
	for i := range ss {
		svc := &ss[i]
		ports, err := FilterServicePorts(svc, portNameOrNumber)
		if err != nil {
			return nil, err
		}
		if (svcName == "" || svc.Name == svcName) && labelsMatch(svc.Spec.Selector) && len(ports) > 0 {
			matching = append(matching, svc)
		}
	}
	return matching, nil
}

func FindServicesSelecting(c context.Context, namespace string, lbs labels.Labels) ([]k8sapi.Object, error) {
	ss, err := k8sapi.Services(c, namespace, nil)
	if err != nil {
		return nil, err
	}
	var ms []k8sapi.Object
	for _, s := range ss {
		if sl, err := s.Selector(); err != nil {
			return nil, err
		} else if sl != nil {
			if sl.Matches(lbs) {
				ms = append(ms, s)
			}
		}
	}
	return ms, nil
}

func FindMatchingService(c context.Context, portNameOrNumber, svcName, namespace string, labels map[string]string) (*core.Service, error) {
	matchingSvcs, err := FindMatchingServices(c, portNameOrNumber, svcName, namespace, labels)
	if err != nil {
		return nil, err
	}
	if len(matchingSvcs) == 1 {
		return matchingSvcs[0], nil
	}

	count := "no"
	suffix := ""
	portRef := ""
	if len(matchingSvcs) > 0 {
		svcNames := make([]string, len(matchingSvcs))
		for i, svc := range matchingSvcs {
			svcNames[i] = svc.Name
		}
		count = "multiple"
		suffix = fmt.Sprintf(", use --service and one of: %s", strings.Join(svcNames, ","))
	}
	if portNameOrNumber != "" {
		portRef = fmt.Sprintf(" and a port referenced by name or port number %s", portNameOrNumber)
	}
	return nil, fmt.Errorf("found %s services with a selector matching labels %v%s in namespace %s%s", count, labels, portRef, namespace, suffix)
}

// FindMatchingPort finds the matching container associated with portNameOrNumber
// in the given service.
func FindMatchingPort(cns []core.Container, portNameOrNumber string, svc *core.Service) (
	sPort *core.ServicePort,
	cn *core.Container,
	cPortIndex int,
	err error,
) {
	// For now, we only support intercepting one port on a given service.
	ports, err := FilterServicePorts(svc, portNameOrNumber)
	if err != nil {
		return nil, nil, 0, err
	}
	switch numPorts := len(ports); {
	case numPorts == 0:
		// this may happen when portNameOrNumber is specified but none of the
		// ports match
		return nil, nil, 0, errors.New("found no Service with a port that matches any container in this workload")

	case numPorts > 1:
		return nil, nil, 0, errors.New(`found matching Service with multiple matching ports.
Please specify the Service port you want to intercept by passing the --port=local:svcPortName flag.`)
	default:
	}
	port := ports[0]
	var matchingServicePort *core.ServicePort
	var matchingContainer *core.Container
	var containerPortIndex int

	if port.TargetPort.Type == intstr.String {
		portName := port.TargetPort.StrVal
		for ci := 0; ci < len(cns) && matchingContainer == nil; ci++ {
			cn := &cns[ci]
			for pi := range cn.Ports {
				if cn.Ports[pi].Name == portName {
					matchingServicePort = &port
					matchingContainer = cn
					containerPortIndex = pi
					break
				}
			}
		}
	} else {
		// First see if we have a container with a matching port
		portNum := port.TargetPort.IntVal
	containerLoop:
		for ci := range cns {
			cn := &cns[ci]
			for pi := range cn.Ports {
				if cn.Ports[pi].ContainerPort == portNum {
					matchingServicePort = &port
					matchingContainer = cn
					containerPortIndex = pi
					break containerLoop
				}
			}
		}
		// If no container matched, then use the first container with no ports at all. This
		// enables intercepts of containers that indeed do listen a port but lack a matching
		// port description in the manifest, which is what you get if you do:
		//     kubectl create deploy my-deploy --image my-image
		//     kubectl expose deploy my-deploy --port 80 --target-port 8080
		if matchingContainer == nil {
			for ci := range cns {
				cn := &cns[ci]
				if len(cn.Ports) == 0 {
					matchingServicePort = &port
					matchingContainer = cn
					containerPortIndex = -1
					break
				}
			}
		}
	}

	if matchingServicePort == nil {
		return nil, nil, 0, errors.New("found no Service with a port that matches any container in this workload")
	}
	return matchingServicePort, matchingContainer, containerPortIndex, nil
}
