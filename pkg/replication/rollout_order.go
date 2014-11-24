package replication

import (
	"sort"

	"github.com/square/p2/pkg/allocation"
	"github.com/square/p2/pkg/health"
)

type compare int

var (
	lessThan    = compare(-1)
	equal       = compare(0)
	greaterThan = compare(1)
)

func (c compare) equal() bool {
	return c == equal
}

func (c compare) less() bool {
	return c == lessThan
}

func compareFromBool(b bool) compare {
	if b {
		return lessThan
	} else {
		return greaterThan
	}
}

type rolloutOrder struct {
	nodes           []allocation.Node
	referenceStatus *health.ServiceStatus
}

func (r *rolloutOrder) Len() int {
	return len(r.nodes)
}

// Sort order is based on a combination of node health and lexical node ordering
// The expected order of the final rollout is:
// [unhealthy nodes, alpha][no-status nodes, alpha][healthy nodes, alpha]
func (r *rolloutOrder) Less(i, j int) bool {
	iNode, jNode := r.nodes[i], r.nodes[j]
	iHealth, iErr := r.referenceStatus.ForNode(iNode.Name)
	jHealth, jErr := r.referenceStatus.ForNode(jNode.Name)

	if comp := r.compareErrors(iErr, jErr); !comp.equal() {
		return comp.less()
	}

	if comp := r.compareHealth(iNode, jNode, iHealth, jHealth); !comp.equal() {
		return comp.less()
	}

	return false
}

func (r *rolloutOrder) compareErrors(iErr, jErr error) compare {
	if iErr == nil && jErr != nil {
		return greaterThan
	}
	if iErr != nil && jErr == nil {
		return lessThan
	}
	if iErr == nil && jErr == nil {
		return equal
	}
	if iErr == jErr && jErr == health.NoStatusGiven {
		return equal
	}
	if iErr == health.NoStatusGiven && jErr != health.NoStatusGiven {
		return greaterThan
	}
	if iErr != health.NoStatusGiven && jErr == health.NoStatusGiven {
		return lessThan
	}
	return equal
}

func (r *rolloutOrder) compareHealth(iNode, jNode allocation.Node, iHealth, jHealth *health.ServiceNodeStatus) compare {
	if iHealth.Healthy && !jHealth.Healthy {
		return greaterThan
	}
	if iHealth.Healthy && jHealth.Healthy {
		return compareFromBool(iNode.Name < jNode.Name)
	}
	if !iHealth.Healthy && jHealth.Healthy {
		return lessThan
	}
	if !iHealth.Healthy && !jHealth.Healthy {
		return compareFromBool(iNode.Name < jNode.Name)
	}
	return equal
}

func (r *rolloutOrder) Swap(i, j int) {
	r.nodes[i], r.nodes[j] = r.nodes[j], r.nodes[i]
}

func getRolloutOrder(alloc allocation.Allocation, status *health.ServiceStatus) chan allocation.Node {
	order := &rolloutOrder{
		nodes:           alloc.Nodes,
		referenceStatus: status,
	}
	sort.Sort(order)
	channel := make(chan allocation.Node, len(alloc.Nodes))
	for _, node := range order.nodes {
		channel <- node
	}
	return channel
}
