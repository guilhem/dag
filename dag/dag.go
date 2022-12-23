package dag

import (
	"fmt"
	"strings"
	"sync"

	"github.com/hashicorp/go-multierror"
)

// AcyclicGraph is a specialization of Graph that cannot have cycles. With
// this property, we get the property of sane graph traversal.
type AcyclicGraph struct {
	Graph
}

// WalkFunc is the callback used for walking the graph.
type WalkFunc func(Vertex) error

// Root returns the root of the DAG, or an error.
//
// Complexity: O(V)
func (g *AcyclicGraph) Root() (Vertex, error) {
	roots := make([]Vertex, 0, 1)
	for _, v := range g.Vertices() {
		if g.UpEdges(v).Len() == 0 {
			roots = append(roots, v)
		}
	}

	if len(roots) > 1 {
		// TODO(mitchellh): make this error message a lot better
		return nil, fmt.Errorf("multiple roots: %#v", roots)
	}

	if len(roots) == 0 {
		return nil, fmt.Errorf("no roots found")
	}

	return roots[0], nil
}

// TransitiveReduction performs the transitive reduction of graph g in place.
// The transitive reduction of a graph is a graph with as few edges as
// possible with the same reachability as the original graph. This means
// that if there are three nodes A => B => C, and A connects to both
// B and C, and B connects to C, then the transitive reduction is the
// same graph with only a single edge between A and B, and a single edge
// between B and C.
//
// The graph must be valid for this operation to behave properly. If
// Validate() returns an error, the behavior is undefined and the results
// will likely be unexpected.
//
// Complexity: O(V(V+E)), or asymptotically O(VE)
func (g *AcyclicGraph) TransitiveReduction() {
	// For each vertex u in graph g, do a DFS starting from each vertex
	// v such that the edge (u,v) exists (v is a direct descendant of u).
	//
	// For each v-prime reachable from v, remove the edge (u, v-prime).

	for _, u := range g.Vertices() {
		uTargets := g.DownEdges(u)
		vs := make([]Vertex, uTargets.Len())
		for i, vRaw := range uTargets.List() {
			vs[i] = vRaw.(Vertex)
		}

		g.depthFirstWalk(vs, func(v Vertex) error {
			shared := uTargets.Intersection(g.DownEdges(v))
			for _, raw := range shared.List() {
				vPrime := raw.(Vertex)
				g.RemoveEdge(BasicEdge(u, vPrime))
			}

			return nil
		})
	}
}

// Validate validates the DAG. A DAG is valid if it has a single root
// with no cycles.
func (g *AcyclicGraph) Validate() error {
	if _, err := g.Root(); err != nil {
		return err
	}

	// Look for cycles of more than 1 component
	var err error
	var cycles [][]Vertex
	for _, cycle := range StronglyConnected(&g.Graph) {
		if len(cycle) > 1 {
			cycles = append(cycles, cycle)
		}
	}
	if len(cycles) > 0 {
		for _, cycle := range cycles {
			cycleStr := make([]string, len(cycle))
			for j, vertex := range cycle {
				cycleStr[j] = VertexName(vertex)
			}

			err = multierror.Append(err, fmt.Errorf(
				"Cycle: %s", strings.Join(cycleStr, ", ")))
		}
	}

	// Look for cycles to self
	for _, e := range g.Edges() {
		if e.Source() == e.Target() {
			err = multierror.Append(err, fmt.Errorf(
				"Self reference: %s", VertexName(e.Source())))
		}
	}

	return err
}

// Walk walks the graph, calling your callback as each node is visited.
// This will walk nodes in parallel if it can. Because the walk is done
// in parallel, the error returned will be a multierror.
func (g *AcyclicGraph) Walk(cb WalkFunc) error {
	// Cache the vertices since we use it multiple times
	vertices := g.Vertices()

	// Build the waitgroup that signals when we're done
	var wg sync.WaitGroup
	wg.Add(len(vertices))
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		wg.Wait()
	}()

	// The map of channels to watch to wait for vertices to finish
	vertMap := make(map[Vertex]chan struct{})
	for _, v := range vertices {
		vertMap[v] = make(chan struct{})
	}

	// The map of whether a vertex errored or not during the walk
	var errLock sync.Mutex
	var errs error
	errMap := make(map[Vertex]bool)
	for _, v := range vertices {
		// Build our list of dependencies and the list of channels to
		// wait on until we start executing for this vertex.
		depsRaw := g.DownEdges(v).List()
		deps := make([]Vertex, len(depsRaw))
		depChs := make([]<-chan struct{}, len(deps))
		for i, raw := range depsRaw {
			deps[i] = raw.(Vertex)
			depChs[i] = vertMap[deps[i]]
		}

		// Get our channel so that we can close it when we're done
		ourCh := vertMap[v]

		// Start the goroutine to wait for our dependencies
		readyCh := make(chan bool)
		go func(deps []Vertex, chs []<-chan struct{}, readyCh chan<- bool) {
			// First wait for all the dependencies
			for _, ch := range chs {
				<-ch
			}

			// Then, check the map to see if any of our dependencies failed
			errLock.Lock()
			defer errLock.Unlock()
			for _, dep := range deps {
				if errMap[dep] {
					readyCh <- false
					return
				}
			}

			readyCh <- true
		}(deps, depChs, readyCh)

		// Start the goroutine that executes
		go func(v Vertex, doneCh chan<- struct{}, readyCh <-chan bool) {
			defer close(doneCh)
			defer wg.Done()

			var err error
			if ready := <-readyCh; ready {
				err = cb(v)
			}

			errLock.Lock()
			defer errLock.Unlock()
			if err != nil {
				errMap[v] = true
				errs = multierror.Append(errs, err)
			}
		}(v, ourCh, readyCh)
	}

	<-doneCh
	return errs
}

// depthFirstWalk does a depth-first walk of the graph starting from
// the vertices in start. This is not exported now but it would make sense
// to export this publicly at some point.
func (g *AcyclicGraph) depthFirstWalk(start []Vertex, cb WalkFunc) error {
	seen := make(map[Vertex]struct{})
	frontier := make([]Vertex, len(start))
	copy(frontier, start)
	for len(frontier) > 0 {
		// Pop the current vertex
		n := len(frontier)
		current := frontier[n-1]
		frontier = frontier[:n-1]

		// Check if we've seen this already and return...
		if _, ok := seen[current]; ok {
			continue
		}
		seen[current] = struct{}{}

		// Visit the current node
		if err := cb(current); err != nil {
			return err
		}

		// Visit targets of this in reverse order.
		targets := g.DownEdges(current).List()
		for i := len(targets) - 1; i >= 0; i-- {
			frontier = append(frontier, targets[i].(Vertex))
		}
	}

	return nil
}
