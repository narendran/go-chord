package buddystore

import (
	"bytes"
	"log"
	"sort"
)

func (r *Ring) init(conf *Config, trans Transport) {
	// Set our variables
	r.config = conf
	r.vnodes = make([]*localVnode, conf.NumVnodes)
	r.transport = InitLocalTransport(trans)
	r.delegateCh = make(chan func(), 32)
	r.shutdownRequested = false
	r.shutdownComplete = make(chan bool, r.config.NumVnodes)

	// Initializes the vnodes
	for i := 0; i < conf.NumVnodes; i++ {
		vn := &localVnode{}
		vn.lm = &LManager{Vn: &vn.Vnode, OpsLog: make([]*OpsLogEntry, 0), CommitPoint: 0, CommitIndex: -1}
		vn.lm.Ring = r // Because the LockManager needs to access transport for Cache Invalidation
		vn.lm_client = &LManagerClient{Vnode: &vn.Vnode, Ring: r, RLocks: make(map[string]*RLockVal), WLocks: make(map[string]*WLockVal)}
		r.vnodes[i] = vn
		vn.ring = r
		vn.init(i)
	}

	// Sort the vnodes
	sort.Sort(r)
}

/* Initialize the LManager with the block flag set to true */
func (r *Ring) initBlockingLM(conf *Config, trans Transport) {
	// Set our variables
	r.config = conf
	r.vnodes = make([]*localVnode, conf.NumVnodes)
	r.transport = InitLocalTransport(trans)
	r.delegateCh = make(chan func(), 32)

	// Initializes the vnodes
	for i := 0; i < conf.NumVnodes; i++ {
		vn := &localVnode{}
		vn.lm = &LManager{Vn: &vn.Vnode, OpsLog: make([]*OpsLogEntry, 0), CommitPoint: 0, CommitIndex: -1, block: true}
		vn.lm.Ring = r // Because the LockManager needs to access transport for Cache Invalidation
		vn.lm_client = &LManagerClient{Vnode: &vn.Vnode, Ring: r, RLocks: make(map[string]*RLockVal), WLocks: make(map[string]*WLockVal)}
		r.vnodes[i] = vn
		vn.ring = r
		vn.init(i)
	}

	// Sort the vnodes
	sort.Sort(r)
}

// Len is the number of vnodes
func (r *Ring) Len() int {
	return len(r.vnodes)
}

// Less returns whether the vnode with index i should sort
// before the vnode with index j.
func (r *Ring) Less(i, j int) bool {
	return bytes.Compare(r.vnodes[i].Id, r.vnodes[j].Id) == -1
}

// Swap swaps the vnodes with indexes i and j.
func (r *Ring) Swap(i, j int) {
	r.vnodes[i], r.vnodes[j] = r.vnodes[j], r.vnodes[i]
}

// Returns the nearest local vnode to the key
func (r *Ring) nearestVnode(key []byte) *localVnode {
	for i := len(r.vnodes) - 1; i >= 0; i-- {
		if bytes.Compare(r.vnodes[i].Id, key) == -1 {
			return r.vnodes[i]
		}
	}
	// Return the last vnode
	return r.vnodes[len(r.vnodes)-1]
}

// Schedules each vnode in the ring
func (r *Ring) schedule() {
	if r.config.Delegate != nil {
		go r.delegateHandler()
	}
	for i := 0; i < len(r.vnodes); i++ {
		r.vnodes[i].schedule()
	}
}

// Signal that the ring is being shut down.
func (r *Ring) requestShutdown() {
	defer r.shutdownLock.Unlock()
	r.shutdownLock.Lock()

	r.shutdownRequested = true
}

// Check if the ring is being shut down.
func (r *Ring) isBeingShutdown() bool {
	defer r.shutdownLock.Unlock()
	r.shutdownLock.Lock()

	return r.shutdownRequested
}

// Wait for all the vnodes to shutdown
func (r *Ring) stopVnodes() {
	r.requestShutdown()
	for i := 0; i < r.config.NumVnodes; i++ {
		<-r.shutdownComplete
	}
}

// Stops the delegate handler
func (r *Ring) stopDelegate() {
	if r.config.Delegate != nil {
		// Wait for all delegate messages to be processed
		<-r.invokeDelegate(r.config.Delegate.Shutdown)
		close(r.delegateCh)
	}
}

// Initializes the vnodes with their local successors
func (r *Ring) setLocalSuccessors() {
	numV := len(r.vnodes)
	numSuc := min(r.config.NumSuccessors, numV-1)
	for idx, vnode := range r.vnodes {
		for i := 0; i < numSuc; i++ {
			vnode.successors[i] = &r.vnodes[(idx+i+1)%numV].Vnode
		}
	}
}

// Initializes the vnodes with their local predecessors
func (r *Ring) setLocalPredecessors() {
	numV := len(r.vnodes)
	numPred := min(r.config.NumSuccessors+1, numV-1)
	for idx, vnode := range r.vnodes {
		// TODO: Consider creating a local list and atomically updating predecessors
		// list.
		vnode.predecessorLock.Lock()

		for i := 0; i < numPred; i++ {
			if (idx - i - 1) < 0 {
				vnode.predecessors[i] = &r.vnodes[numV-i-1].Vnode
			} else {
				vnode.predecessors[i] = &r.vnodes[idx-i-1].Vnode
			}
		}

		vnode.predecessorLock.Unlock()
	}
}

// Invokes a function on the delegate and returns completion channel
func (r *Ring) invokeDelegate(f func()) chan struct{} {
	if r.config.Delegate == nil {
		return nil
	}

	ch := make(chan struct{}, 1)
	wrapper := func() {
		defer func() {
			ch <- struct{}{}
		}()
		f()
	}

	r.delegateCh <- wrapper
	return ch
}

// This handler runs in a go routine to invoke methods on the delegate
func (r *Ring) delegateHandler() {
	for {
		f, ok := <-r.delegateCh
		if !ok {
			break
		}
		r.safeInvoke(f)
	}
}

// Called to safely call a function on the delegate
func (r *Ring) safeInvoke(f func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Caught a panic invoking a delegate function! Got: %s", r)
		}
	}()
	f()
}
