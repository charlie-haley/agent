package flow

import (
	"github.com/grafana/agent/pkg/flow/dag"
	"github.com/grafana/agent/pkg/flow/rawcomponent"
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// node is a flow component and a node in the DAG.
type node interface {
	dag.Node
}

// componentNode is a lazily-constructed component.
type componentNode struct {
	ref   reference
	block *hcl.Block

	raw rawcomponent.Component
}

var _ node = (*componentNode)(nil)

// newComponentNode constructs a componentNode from a block.
func newComponentNode(block *hcl.Block) *componentNode {
	ref := make(reference, 0, 1+len(block.Labels))
	ref = append(ref, block.Type)
	ref = append(ref, block.Labels...)

	return &componentNode{
		ref:   ref,
		block: block,
	}
}

func (cn *componentNode) Reference() reference {
	return cn.ref
}

func (cn *componentNode) Name() string {
	return cn.ref.String()
}

func (cn *componentNode) CurrentState() cty.Value {
	return cn.raw.CurrentState()
}

func (cn *componentNode) Set(rc rawcomponent.Component) {
	cn.raw = rc
}
