package dag

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
)

// walker performs a graph walk
type walker struct {
	Callback WalkFunc

	vertices  *Set
	edges     *Set
	vertexMap map[Vertex]*walkerVertex

	wait       sync.WaitGroup
	changeLock sync.Mutex

	errMap  map[Vertex]error
	errLock sync.Mutex
}

type walkerVertex struct {
	sync.Mutex

	DoneCh       chan struct{}
	CancelCh     chan struct{}
	DepsCh       chan struct{}
	DepsUpdateCh chan chan struct{}

	deps         map[Vertex]chan struct{}
	depsCancelCh chan struct{}
}

// Wait waits for the completion of the walk and returns any errors (
// in the form of a multierror) that occurred. Update should be called
// to populate the walk with vertices and edges.
func (w *walker) Wait() error {
	// Wait for completion
	w.wait.Wait()

	// Grab the error lock
	w.errLock.Lock()
	defer w.errLock.Unlock()

	// Build the error
	var result error
	for v, err := range w.errMap {
		result = multierror.Append(result, fmt.Errorf(
			"%s: %s", VertexName(v), err))
	}

	return result
}

// Update updates the currently executing walk with the given vertices
// and edges. It does not block until completion.
//
// Update can be called in parallel to Walk.
func (w *walker) Update(v, e *Set) {
	// Grab the change lock so no more updates happen but also so that
	// no new vertices are executed during this time since we may be
	// removing them.
	w.changeLock.Lock()
	defer w.changeLock.Unlock()

	// Initialize fields
	if w.vertexMap == nil {
		w.vertexMap = make(map[Vertex]*walkerVertex)
	}

	// Calculate all our sets
	newEdges := e.Difference(w.edges)
	newVerts := v.Difference(w.vertices)
	oldVerts := w.vertices.Difference(v)

	// Add the new vertices
	for _, raw := range newVerts.List() {
		v := raw.(Vertex)

		// Add to the waitgroup so our walk is not done until everything finishes
		w.wait.Add(1)

		// Initialize the vertex info
		info := &walkerVertex{
			DoneCh:       make(chan struct{}),
			CancelCh:     make(chan struct{}),
			DepsCh:       make(chan struct{}),
			DepsUpdateCh: make(chan chan struct{}, 5),
			deps:         make(map[Vertex]chan struct{}),
		}

		// Close the deps channel immediately so it passes
		close(info.DepsCh)

		// Add it to the map and kick off the walk
		w.vertexMap[v] = info
	}

	// Remove the old vertices
	for _, raw := range oldVerts.List() {
		v := raw.(Vertex)

		// Get the vertex info so we can cancel it
		info, ok := w.vertexMap[v]
		if !ok {
			// This vertex for some reason was never in our map. This
			// shouldn't be possible.
			continue
		}

		// Cancel the vertex
		close(info.CancelCh)

		// Delete it out of the map
		delete(w.vertexMap, v)
	}

	// Add the new edges
	var changedDeps Set
	for _, raw := range newEdges.List() {
		edge := raw.(Edge)

		// waiter is the vertex that is "waiting" on this edge
		waiter := edge.Target()

		// dep is the dependency we're waiting on
		dep := edge.Source()

		// Get the info for the waiter
		waiterInfo, ok := w.vertexMap[waiter]
		if !ok {
			// Vertex doesn't exist... shouldn't be possible but ignore.
			continue
		}

		// Get the info for the dep
		depInfo, ok := w.vertexMap[dep]
		if !ok {
			// Vertex doesn't exist... shouldn't be possible but ignore.
			continue
		}

		// Add the dependency to our waiter
		waiterInfo.deps[dep] = depInfo.DoneCh

		// Record that the deps changed for this waiter
		changedDeps.Add(waiter)
	}

	for _, raw := range changedDeps.List() {
		v := raw.(Vertex)
		info, ok := w.vertexMap[v]
		if !ok {
			// Vertex doesn't exist... shouldn't be possible but ignore.
			continue
		}

		// Create a new done channel
		doneCh := make(chan struct{})

		// Create the channel we close for cancellation
		cancelCh := make(chan struct{})
		info.depsCancelCh = cancelCh

		// Build a new deps copy
		deps := make(map[Vertex]<-chan struct{})
		for k, v := range info.deps {
			deps[k] = v
		}

		// Update the update channel
		info.DepsUpdateCh <- doneCh

		// Start the waiter
		go w.waitDeps(v, deps, doneCh, cancelCh)
	}

	// Kickstart all the vertices
	for _, raw := range newVerts.List() {
		v := raw.(Vertex)
		go w.walkVertex(v, w.vertexMap[v])
	}
}

// walkVertex walks a single vertex, waiting for any dependencies before
// executing the callback.
func (w *walker) walkVertex(v Vertex, info *walkerVertex) {
	// When we're done executing, lower the waitgroup count
	defer w.wait.Done()

	// When we're done, always close our done channel
	defer close(info.DoneCh)

	// Wait for our dependencies
	depsCh := info.DepsCh
	for {
		select {
		case <-info.CancelCh:
			// Cancel
			return

		case <-depsCh:
			// Deps complete!
			depsCh = nil

		case depsCh = <-info.DepsUpdateCh:
			// New deps, reloop
		}

		if depsCh == nil {
			// One final check if we have an update
			select {
			case depsCh = <-info.DepsUpdateCh:
			default:
			}

			if depsCh == nil {
				break
			}
		}
	}

	// Call our callback
	if err := w.Callback(v); err != nil {
		w.errLock.Lock()
		defer w.errLock.Unlock()

		if w.errMap == nil {
			w.errMap = make(map[Vertex]error)
		}
		w.errMap[v] = err
	}
}

func (w *walker) waitDeps(
	v Vertex,
	deps map[Vertex]<-chan struct{},
	doneCh chan<- struct{},
	cancelCh <-chan struct{}) {
	// Whenever we return, mark ourselves as complete
	defer close(doneCh)

	// For each dependency given to us, wait for it to complete
	for dep, depCh := range deps {
	DepSatisfied:
		for {
			select {
			case <-depCh:
				// Dependency satisfied!
				break DepSatisfied

			case <-cancelCh:
				// Wait cancelled
				return

			case <-time.After(time.Second * 5):
				log.Printf("[DEBUG] vertex %q, waiting for: %q",
					VertexName(v), VertexName(dep))
			}
		}
	}
}
