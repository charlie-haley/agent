// Package flow implements a component graph system.
package flow

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/agent/component"
	"github.com/grafana/agent/pkg/flow/dag"
	"github.com/grafana/agent/pkg/flow/graphviz"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
)

// Flow is the Flow component graph system.
type Flow struct {
	log        log.Logger
	configFile string

	graphMut  sync.RWMutex
	graph     *dag.Graph
	nametable *nametable
}

// New creates a new Flow instance.
func New(l log.Logger, configFile string) *Flow {
	f := &Flow{
		log:        l,
		configFile: configFile,
		graph:      &dag.Graph{},
		nametable:  &nametable{},
	}
	return f
}

// Load reads the config file and updates the system to reflect what was read.
func (f *Flow) Load() error {
	f.graphMut.Lock()
	defer f.graphMut.Unlock()

	// TODO(rfratto): this won't work yet for subsequent loads.
	//
	// Figuring out how to mutate the DAG to match the current state of the file
	// will take some thinking.

	bb, err := os.ReadFile(f.configFile)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	file, diags := hclsyntax.ParseConfig(bb, f.configFile, hcl.InitialPos)
	if diags.HasErrors() {
		return diags
	}

	var root rootBlock
	decodeDiags := gohcl.DecodeBody(file.Body, nil, &root)
	diags = diags.Extend(decodeDiags)
	if diags.HasErrors() {
		return diags
	}

	blockSchema := component.RegistrySchema()
	content, remainDiags := root.Remain.Content(blockSchema)
	diags = diags.Extend(remainDiags)
	if diags.HasErrors() {
		return diags
	}

	// Construct our components and the nametable.
	for _, block := range content.Blocks {
		// Create the component and add it into our graph.
		component := newComponentNode(block)
		f.graph.Add(component)

		// Then, add the component into our nametable.
		f.nametable.Add(component)
	}

	// Second pass: iterate over all of our nodes and create edges.
	for _, node := range f.graph.Nodes() {
		var (
			component  = node.(*componentNode)
			body       = component.block.Body
			traversals = expressionsFromSyntaxBody(body.(*hclsyntax.Body))
		)
		for _, t := range traversals {
			target, lookupDiags := f.nametable.LookupTraversal(t)
			diags = diags.Extend(lookupDiags)
			if target == nil {
				continue
			}

			// Add dependency to the found node
			f.graph.AddEdge(dag.Edge{From: component, To: target})
		}
	}
	if diags.HasErrors() {
		return diags
	}

	// Wiring edges probably caused a mess. Reduce it.
	dag.Reduce(f.graph)

	funcMap := map[string]function.Function{
		"concat": stdlib.ConcatFunc,
	}

	// At this point, our DAG is completely formed and we can start to construct
	// the real components and evaluate expressions. Walk topologically in
	// dependency order.
	//
	// TODO(rfratto): should this happen as part of the run? If we moved this to
	// the run, we would need a separate type checking pass in the Load to ensure
	// that all expressions thoughout the config are valid. As it is now, this
	// typechecks on its own.
	err = dag.WalkTopological(f.graph, f.graph.Leaves(), func(n dag.Node) error {
		cn := n.(*componentNode)

		directDeps := f.graph.Dependencies(cn)
		ectx, err := f.nametable.BuildEvalContext(directDeps)
		if err != nil {
			return err
		} else if ectx != nil {
			ectx.Functions = funcMap
		}

		bctx := &component.BuildContext{
			Log:         log.With(f.log, "node", cn.Name()),
			EvalContext: ectx,
		}

		componentID := cn.ref[:len(cn.ref)-1]
		rc, err := component.BuildHCL(componentID.String(), bctx, cn.block)
		if err != nil {
			return err
		}

		cn.Set(rc)
		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

type rootBlock struct {
	LogLevel  string `hcl:"log_level,optional"`
	LogFormat string `hcl:"log_format,optional"`

	Body   hcl.Body `hcl:",body"`
	Remain hcl.Body `hcl:",remain"`
}

type reference []string

func (r reference) String() string {
	return strings.Join(r, ".")
}

// expressionsFromSyntaxBody returcses through body and finds all variable
// references.
func expressionsFromSyntaxBody(body *hclsyntax.Body) []hcl.Traversal {
	var exprs []hcl.Traversal

	for _, attrib := range body.Attributes {
		exprs = append(exprs, attrib.Expr.Variables()...)
	}
	for _, block := range body.Blocks {
		exprs = append(exprs, expressionsFromSyntaxBody(block.Body)...)
	}

	return exprs
}

// Run runs f until ctx is canceled. It is invalid to call Run concurrently.
func (f *Flow) Run(ctx context.Context) error {
	funcMap := map[string]function.Function{
		"concat": stdlib.ConcatFunc,
	}

	refreshCh := make(chan struct{}, 1)
	var updated sync.Map

	// TODO(rfratto): start/stop nodes after refresh
	var wg sync.WaitGroup
	defer wg.Wait()

	f.graphMut.Lock()
	for _, n := range f.graph.Nodes() {
		cn := n.(*componentNode)
		if cn.raw == nil {
			return fmt.Errorf("componentNode %q not initialized", cn.Name())
		}

		wg.Add(1)
		go func(cn *componentNode) {
			defer wg.Done()

			err := cn.raw.Run(ctx, func() {
				updated.Store(cn, struct{}{})

				select {
				case refreshCh <- struct{}{}:
				default:
				}
			})
			if err != nil {
				level.Error(f.log).Log("msg", "node exited with error", "node", cn.Name(), "err", err)
			}
		}(cn)
	}
	f.graphMut.Unlock()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-refreshCh:
			updated.Range(func(key, _ interface{}) bool {
				defer updated.Delete(key)

				cn := key.(*componentNode)
				level.Debug(f.log).Log("msg", "handling node with updated state", "node", cn.Name())

				f.graphMut.Lock()
				defer f.graphMut.Unlock()

				// Update dependants
				// TODO(rfratto): set health of node based on result of this?
				for _, n := range f.graph.Dependants(cn) {
					cn := n.(*componentNode)

					directDeps := f.graph.Dependencies(cn)
					ectx, err := f.nametable.BuildEvalContext(directDeps)
					if err != nil {
						level.Error(f.log).Log("msg", "failed to update node", "node", cn.Name(), "err", err)
						continue
					} else if ectx != nil {
						ectx.Functions = funcMap
					}

					if err := cn.raw.Update(ectx, cn.block); err != nil {
						level.Error(f.log).Log("msg", "failed to update node", "node", cn.Name(), "err", err)
						continue
					}
				}

				return true
			})
		}
	}
}

// GraphHandler returns an http.HandlerFunc that render's the flow's DAG as an
// SVG. Graphviz must be installed for this to work.
func GraphHandler(f *Flow) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		f.graphMut.RLock()
		contents := dag.MarshalDOT(f.graph)
		f.graphMut.RUnlock()

		svgBytes, err := graphviz.Dot(contents, "svg")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = io.Copy(w, bytes.NewReader(svgBytes))
	}
}

// GraphHandler returns an http.HandlerFunc that render's the flow's nametable
// as an SVG. Graphviz must be installed for this to work.
func NametableHandler(f *Flow) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		f.graphMut.RLock()
		contents := dag.MarshalDOT(&f.nametable.graph)
		f.graphMut.RUnlock()

		svgBytes, err := graphviz.Dot(contents, "svg")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = io.Copy(w, bytes.NewReader(svgBytes))
	}
}
