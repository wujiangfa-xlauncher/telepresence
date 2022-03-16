package trafficmgr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/datawire/dlib/dlog"
	"github.com/datawire/dlib/dtime"
	rpc "github.com/telepresenceio/telepresence/rpc/v2/connector"
	"github.com/telepresenceio/telepresence/rpc/v2/manager"
	"github.com/telepresenceio/telepresence/rpc/v2/userdaemon"
	"github.com/telepresenceio/telepresence/v2/pkg/client"
	"github.com/telepresenceio/telepresence/v2/pkg/k8sapi"
)

// getCurrentAgents returns a copy of the current agent snapshot
func (tm *TrafficManager) getCurrentAgents() []*manager.AgentInfo {
	// Copy the current snapshot
	tm.currentAgentsLock.Lock()
	agents := make([]*manager.AgentInfo, len(tm.currentAgents))
	for i, ii := range tm.currentAgents {
		agents[i] = proto.Clone(ii).(*manager.AgentInfo)
	}
	tm.currentAgentsLock.Unlock()
	return agents
}

// getCurrentAgentsInNamespace returns a map of agents matching the given namespace from the current agent snapshot.
// The map contains the first agent for each name found. Agents from replicas of the same workload are ignored.
func (tm *TrafficManager) getCurrentAgentsInNamespace(ns string) map[string]*manager.AgentInfo {
	// Copy the current snapshot
	tm.currentAgentsLock.Lock()
	agents := make(map[string]*manager.AgentInfo)
	for _, ii := range tm.currentAgents {
		if ii.Namespace == ns {
			// There may be any number or replicas of the agent. Avoid cloning all of them.
			if _, ok := agents[ii.Name]; !ok {
				agents[ii.Name] = proto.Clone(ii).(*manager.AgentInfo)
			}
		}
	}
	tm.currentAgentsLock.Unlock()
	return agents
}

func (tm *TrafficManager) setCurrentAgents(agents []*manager.AgentInfo) {
	tm.currentAgentsLock.Lock()
	tm.currentAgents = agents
	tm.currentAgentsLock.Unlock()
}

func (tm *TrafficManager) agentInfoWatcher(ctx context.Context) error {
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		stream, err := tm.managerClient.WatchAgents(ctx, tm.session())
		if err != nil {
			err = fmt.Errorf("manager.WatchAgents dial: %w", err)
		}
		for err == nil && ctx.Err() == nil {
			snapshot, err := stream.Recv()
			if err != nil {
				if ctx.Err() == nil && !errors.Is(err, io.EOF) {
					dlog.Errorf(ctx, "manager.WatchAgents recv: %v", err)
				}
				tm.setCurrentAgents(nil)
				break
			}
			tm.setCurrentAgents(snapshot.Agents)

			// Notify waiters for agents
			for _, agent := range snapshot.Agents {
				fullName := agent.Name + "." + agent.Namespace
				if chUt, loaded := tm.agentWaiters.LoadAndDelete(fullName); loaded {
					if ch, ok := chUt.(chan *manager.AgentInfo); ok {
						dlog.Debugf(ctx, "wait status: agent %s arrived", fullName)
						ch <- agent
						close(ch)
					}
				}
			}
		}

		dtime.SleepWithContext(ctx, backoff)
		backoff *= 2
		if backoff > 3*time.Second {
			backoff = 3 * time.Second
		}
	}
	return nil
}

func (tm *TrafficManager) addAgent(
	c context.Context,
	workload k8sapi.Workload,
	svcprops *ServiceProps,
	agentImageName string,
	telepresenceAPIPort uint16,
) *rpc.InterceptResult {
	agentName := workload.GetName()
	namespace := workload.GetNamespace()
	svcUID, kind, err := tm.EnsureAgent(c, workload, svcprops, agentImageName, telepresenceAPIPort)
	if err != nil {
		if err == agentNotFound {
			return &rpc.InterceptResult{
				Error:     rpc.InterceptError_NOT_FOUND,
				ErrorText: agentName,
			}
		}
		dlog.Error(c, err)
		return &rpc.InterceptResult{
			Error:     rpc.InterceptError_FAILED_TO_ESTABLISH,
			ErrorText: err.Error(),
		}
	}

	dlog.Infof(c, "Waiting for agent for %s %s.%s", kind, agentName, namespace)
	if _, err = tm.waitForAgent(c, agentName, namespace); err != nil {
		dlog.Error(c, err)
		return &rpc.InterceptResult{
			Error:     rpc.InterceptError_FAILED_TO_ESTABLISH,
			ErrorText: err.Error(),
		}
	}
	dlog.Infof(c, "Agent found or created for %s %s.%s", kind, agentName, namespace)
	return &rpc.InterceptResult{
		Error:        rpc.InterceptError_UNSPECIFIED,
		ServiceUid:   svcUID,
		WorkloadKind: kind,
		ServiceProps: &userdaemon.IngressInfoRequest{
			ServiceUid:            svcUID,
			ServiceName:           svcprops.Service.Name,
			ServicePortIdentifier: string(svcprops.ServicePort.Port),
			ServicePort:           svcprops.ServicePort.Port,
			Namespace:             namespace,
		},
	}
}

func (tm *TrafficManager) waitForAgent(ctx context.Context, name, namespace string) (*manager.AgentInfo, error) {
	fullName := name + "." + namespace
	waitCh := make(chan *manager.AgentInfo)
	tm.agentWaiters.Store(fullName, waitCh)
	defer tm.agentWaiters.Delete(fullName)

	// Agent may already exist.
	for _, agent := range tm.getCurrentAgentsInNamespace(namespace) {
		if agent.Name == name {
			return agent, nil
		}
	}

	ctx, cancel := client.GetConfig(ctx).Timeouts.TimeoutContext(ctx, client.TimeoutAgentInstall) // installing a new agent can take some time
	defer cancel()

	select {
	case <-ctx.Done():
		return nil, client.CheckTimeout(ctx, fmt.Errorf("waiting for agent %q to be present", fullName))
	case agent := <-waitCh:
		return agent, nil
	}
}
