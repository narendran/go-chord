package buddystore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"time"
)

// Converts the ID to string
func (vn *Vnode) String() string {
	return fmt.Sprintf("%x", vn.Id)
}

// Initializes a local vnode
func (vn *localVnode) init(idx int) {
	// Generate an ID
	vn.genId(uint16(idx))

	// Set our host
	vn.Host = vn.ring.config.Hostname

	// Initialize all state
	vn.successors = make([]*Vnode, vn.ring.config.NumSuccessors)
	vn.predecessors = make([]*Vnode, vn.ring.config.NumSuccessors+1)
	vn.finger = make([]*Vnode, vn.ring.config.hashBits)

	// Register with the RPC mechanism
	vn.ring.transport.Register(&vn.Vnode, vn)

	// Initialise the key-value store
	vn.store = &KVStore{}
	vn.store.vn = vn
	vn.store.init()

	// Initialize the tracker server
	// TODO: Should we check this ring supports a tracker server?
	vn.tracker = NewTrackerWithStore(NewKVStoreClientWithLM(vn.Ring(), vn.lm_client))
}

// Schedules the Vnode to do regular maintenence
func (vn *localVnode) schedule() {
	// Setup our stabilize timer
	defer vn.timerLock.Unlock()
	vn.timerLock.Lock()
	vn.timer = time.AfterFunc(randStabilize(vn.ring.config), vn.stabilize)
}

// Generates an ID for the node
func (vn *localVnode) genId(idx uint16) {
	// Use the hash funciton
	conf := vn.ring.config
	hash := conf.HashFunc()
	hash.Write([]byte(conf.Hostname))
	binary.Write(hash, binary.BigEndian, idx)

	// Use the hash as the ID
	vn.Id = hash.Sum(nil)
}

// Clear reschedule timer
func (vn *localVnode) clearTimer() {
	defer vn.timerLock.Unlock()
	vn.timerLock.Lock()
	vn.timer = nil
}

// Called to periodically stabilize the vnode
func (vn *localVnode) stabilize() {
	vn.clearTimer()

	// Check for shutdown
	if vn.ring.isBeingShutdown() {
		vn.ring.shutdownComplete <- true
		return
	}

	// Setup the next stabilize timer
	defer vn.schedule()

	// Check for new successor
	if err := vn.checkNewSuccessor(); err != nil {
		log.Printf("[ERR] Error checking for new successor: %s", err)
	}

	// Notify the successor
	if err := vn.notifySuccessor(); err != nil {
		log.Printf("[ERR] Error notifying successor: %s", err)
	}

	// Finger table fix up
	if err := vn.fixFingerTable(); err != nil {
		log.Printf("[ERR] Error fixing finger table: %s", err)
	}

	// Check the predecessor
	if err := vn.checkPredecessor(); err != nil {
		log.Printf("[ERR] Error checking predecessor: %s", err)
	}

	// Update the predecessor list
	if err := vn.updatePredecessorList(); err != nil {
		log.Printf("[ERR] Error updating predecessor list: %s", err)
	}

	// Locking predecessors because we're passing predecessors by reference.
	// TODO: Make a copy of predecessors and successors here.

	vn.predecessorLock.RLock()
	vn.successorsLock.RLock()
	vn.store.updatePredSuccList(vn.predecessors, vn.successors)
	vn.successorsLock.RUnlock()
	vn.predecessorLock.RUnlock()

	go vn.store.localRepl()
	go vn.store.globalRepl()

	// Set the last stabilized time
	vn.stabilized = time.Now()

}

// Checks for a new successor
func (vn *localVnode) checkNewSuccessor() error {
	// Ask our successor for it's predecessor
	trans := vn.ring.transport

	vn.successorsLock.Lock()
	defer vn.successorsLock.Unlock()

CHECK_NEW_SUC:
	succ := vn.successors[0]
	if succ == nil {
		panic("Node has no successor!")
	}
	maybe_suc, err := trans.GetPredecessor(succ)
	if err != nil {
		// Check if we have succ list, try to contact next live succ
		known := vn.knownSuccessors()
		if known > 1 {
			for i := 0; i < known; i++ {
				if alive, _ := trans.Ping(vn.successors[0]); !alive {
					// Don't eliminate the last successor we know of
					if i+1 == known {
						return fmt.Errorf("All known successors dead!")
					}

					// Advance the successors list past the dead one
					copy(vn.successors[0:], vn.successors[1:])
					vn.successors[known-1-i] = nil
				} else {
					// Found live successor, check for new one
					goto CHECK_NEW_SUC
				}
			}
		}
		return err
	}

	// Check if we should replace our successor
	if maybe_suc != nil && between(vn.Id, succ.Id, maybe_suc.Id) {
		// Check if new successor is alive before switching
		alive, err := trans.Ping(maybe_suc)
		if alive && err == nil {
			copy(vn.successors[1:], vn.successors[0:len(vn.successors)-1])
			vn.successors[0] = maybe_suc
		} else {
			return err
		}
	}
	return nil
}

// RPC: Invoked to return out predecessor
func (vn *localVnode) GetPredecessor() (*Vnode, error) {
	defer vn.predecessorLock.RUnlock()
	vn.predecessorLock.RLock()

	return vn.predecessor, nil
}

// RPC Short Circuit: Return predecessor list
// Must be called with predecessorLock held
// TODO: Is there a way to assert that the lock is held?
func (vn *localVnode) GetPredecessorList() ([]*Vnode, error) {
	var retPredList []*Vnode

	retPredList = make([]*Vnode, 0, vn.ring.config.NumSuccessors+1)

	defer vn.predecessorLock.RUnlock()
	vn.predecessorLock.RLock()

	for i := 0; i < vn.ring.config.NumSuccessors+1; i++ {
		if vn.predecessors[i] != nil {
			retPredList = append(retPredList, vn.predecessors[i])
		}
	}

	return retPredList, nil
}

// Notifies our successor of us, updates successor list
func (vn *localVnode) notifySuccessor() error {
	// Notify successor
	vn.successorsLock.RLock()
	succ := vn.successors[0]
	vn.successorsLock.RUnlock()
	succ_list, err := vn.ring.transport.Notify(succ, &vn.Vnode)
	if err != nil {
		return err
	}

	// Trim the successors list if too long
	max_succ := vn.ring.config.NumSuccessors
	if len(succ_list) > max_succ-1 {
		succ_list = succ_list[:max_succ-1]
	}

	// Update local successors list
	for idx, s := range succ_list {
		if s == nil {
			break
		}
		// Ensure we don't set ourselves as a successor!
		if s == nil || s.String() == vn.String() {
			break
		}
		vn.successorsLock.Lock()
		vn.successors[idx+1] = s
		vn.successorsLock.Unlock()
	}
	return nil
}

// RPC: Notify is invoked when a Vnode gets notified
func (vn *localVnode) Notify(maybe_pred *Vnode) ([]*Vnode, error) {
	defer vn.predecessorLock.Unlock()
	vn.predecessorLock.Lock()

	// Check if we should update our predecessor
	if vn.predecessor == nil || between(vn.predecessor.Id, vn.Id, maybe_pred.Id) {
		// Inform the delegate
		conf := vn.ring.config
		old := vn.predecessor
		vn.ring.invokeDelegate(func() {
			conf.Delegate.NewPredecessor(&vn.Vnode, maybe_pred, old)
		})

		// If there is a change in the predecessor, my LockManager status might change.
		if vn.lm != nil && vn.lm.Ring != nil {
			if !vn.lm.block { // If you are supposed to be blocking, do not start any activity yet
				nearestNode := vn.lm.Ring.nearestVnode([]byte(vn.lm.Ring.config.RingId))

				nearestNode.successorsLock.RLock()
				defer nearestNode.successorsLock.RUnlock()

				if nearestNode.successors[0] != nil {
					if (vn.predecessor == nil && maybe_pred != nil) || bytes.Compare(vn.predecessor.Id, maybe_pred.Id) != 0 {
						LMVnodes, err := vn.lm.Ring.Lookup(1, []byte(vn.lm.Ring.config.RingId))
						if err != nil {
							fmt.Println("Lookup for LockManager failed with error ", err)
						}

						/* Once a lock manager starts operating, it should care about only two possibilies in terms of failure handling
						   1. Node joining as its predecessor and becoming the LM
						   2. Node dying before it and making it the LM or It just joined and found that it is the LM, in which case its opslog will be empty
						*/
						if vn.String() == LMVnodes[0].String() {
							if vn.lm.CurrentLM {
								// No-op
							} else {
								vn.lm.SyncWithSuccessor()
								vn.lm.ReplayLog()
								vn.lm.CurrentLM = true
							}
						} else {
							if vn.lm.CurrentLM {
								fmt.Println("Lost LockManager status, sending Lock context to current LM")
								resp := tcpVersionMapUpdateResp{}
								err := vn.ring.transport.(*LocalTransport).remote.(*TCPTransport).networkCall(LMVnodes[0].Host, tcpVersionMapUpdate, tcpVersionMapUpdateReq{Vn: LMVnodes[0], VersionMap: &vn.lm.VersionMap}, &resp)

								if err != nil {
									fmt.Errorf("Error while trying to provide Lock context to the new LockManager : ", err)
								}
								vn.lm.CurrentLM = false
							} else {
								// No-op
							}
						}
					}
				}
			}
		}
		vn.predecessor = maybe_pred
		vn.predecessors[0] = maybe_pred
	}

	// Return our successors list
	return vn.CopyOfSuccessors(), nil
}

// Fixes up the finger table
func (vn *localVnode) fixFingerTable() error {
	// Determine the offset
	hb := vn.ring.config.hashBits
	offset := powerOffset(vn.Id, vn.last_finger, hb)

	// Find the successor
	nodes, err := vn.FindSuccessors(1, offset)
	if nodes == nil || len(nodes) == 0 || err != nil {
		return err
	}
	node := nodes[0]

	defer vn.fingerLock.Unlock()
	vn.fingerLock.Lock()
	// Update the finger table
	vn.finger[vn.last_finger] = node

	// Try to skip as many finger entries as possible
	for {
		next := vn.last_finger + 1
		if next >= hb {
			break
		}
		offset := powerOffset(vn.Id, next, hb)

		// While the node is the successor, update the finger entries
		if betweenRightIncl(vn.Id, node.Id, offset) {
			vn.finger[next] = node
			vn.last_finger = next
		} else {
			break
		}
	}

	// Increment to the index to repair
	if vn.last_finger+1 == hb {
		vn.last_finger = 0
	} else {
		vn.last_finger++
	}

	return nil
}

// Checks the health of our predecessor
func (vn *localVnode) checkPredecessor() error {
	defer vn.predecessorLock.Unlock()
	vn.predecessorLock.Lock()

	// Check predecessor
	if vn.predecessor != nil {
		res, err := vn.ring.transport.Ping(vn.predecessor)
		if err != nil {
			return err
		}

		// Predecessor is dead
		if !res {
			vn.predecessor = nil
			vn.predecessors[0] = nil
		}
	}
	return nil
}

// Update the predecessor list
func (vn *localVnode) updatePredecessorList() error {
	if vn.predecessor != nil {
		pred_list, err := vn.ring.transport.GetPredecessorList(vn.predecessor)
		if err != nil {
			return err
		}

		// Trim the predecessors list if too long
		max_pred := vn.ring.config.NumSuccessors + 1
		if len(pred_list) > max_pred-1 {
			pred_list = pred_list[:max_pred-1]
		}

		defer vn.predecessorLock.Unlock()
		vn.predecessorLock.Lock()

		// Update local predecessors list
		for idx, p := range pred_list {
			if p == nil {
				break
			}
			// Ensure we don't set ourselves as a predecessor!
			if p == nil || p.String() == vn.String() {
				break
			}
			vn.predecessors[idx+1] = p
		}
	}

	return nil
}

// Finds next N successors. N must be <= NumSuccessors
func (vn *localVnode) FindSuccessors(n int, key []byte) ([]*Vnode, error) {
	// Check if we are the immediate predecessor

	vn.successorsLock.RLock()
	defer vn.successorsLock.RUnlock()

	if betweenRightIncl(vn.Id, vn.successors[0].Id, key) {
		return copyOfVnodesList(vn.successors, n), nil
	}

	// Try the closest preceeding nodes
	cp := closestPreceedingVnodeIterator{}
	cp.init(vn, key)
	for {
		// Get the next closest node
		closest := cp.Next()
		if closest == nil {
			break
		}

		// Try that node, break on success
		res, err := vn.ring.transport.FindSuccessors(closest, n, key)
		if err == nil {
			return res, nil
		} else {
			log.Printf("[ERR] Failed to contact %s. Got %s", closest.String(), err)
		}
	}

	// Determine how many successors we know of
	successors := vn.knownSuccessors()

	// Check if the ID is between us and any non-immediate successors
	for i := 1; i <= successors-n; i++ {
		if betweenRightIncl(vn.Id, vn.successors[i].Id, key) {
			remain := vn.successors[i:]
			if len(remain) > n {
				remain = remain[:n]
			}
			return copyOfVnodesList(remain, len(remain)), nil
		}
	}

	// Checked all closer nodes and our successors!
	return nil, fmt.Errorf("Exhausted all preceeding nodes!")
}

// Instructs the vnode to leave
func (vn *localVnode) leave() error {
	defer vn.predecessorLock.RUnlock()
	vn.predecessorLock.RLock()

	// Inform the delegate we are leaving
	conf := vn.ring.config
	pred := vn.predecessor
	succ := vn.successors[0]
	vn.ring.invokeDelegate(func() {
		conf.Delegate.Leaving(&vn.Vnode, pred, succ)
	})

	// Notify predecessor to advance to their next successor
	var err error
	trans := vn.ring.transport
	if vn.predecessor != nil {
		err = trans.SkipSuccessor(vn.predecessor, &vn.Vnode)
	}

	// Notify successor to clear old predecessor
	err = mergeErrors(err, trans.ClearPredecessor(vn.successors[0], &vn.Vnode))
	return err
}

// Used to clear our predecessor when a node is leaving
func (vn *localVnode) ClearPredecessor(p *Vnode) error {
	defer vn.predecessorLock.Unlock()
	vn.predecessorLock.Lock()

	if vn.predecessor != nil && vn.predecessor.String() == p.String() {
		// Inform the delegate
		conf := vn.ring.config
		old := vn.predecessor
		vn.ring.invokeDelegate(func() {
			conf.Delegate.PredecessorLeaving(&vn.Vnode, old)
		})
		vn.predecessor = nil

		vn.predecessors[0] = nil
	}

	return nil
}

// Used to skip a successor when a node is leaving
func (vn *localVnode) SkipSuccessor(s *Vnode) error {
	vn.successorsLock.Lock()
	defer vn.successorsLock.Unlock()

	// Skip if we have a match
	if vn.successors[0].String() == s.String() {
		// Inform the delegate
		conf := vn.ring.config
		old := vn.successors[0]
		vn.ring.invokeDelegate(func() {
			conf.Delegate.SuccessorLeaving(&vn.Vnode, old)
		})

		known := vn.knownSuccessors()
		copy(vn.successors[0:], vn.successors[1:])
		vn.successors[known-1] = nil
	}
	return nil
}

// Determine how many successors we know of
func (vn *localVnode) knownSuccessors() (successors int) {
	for i := 0; i < len(vn.successors); i++ {
		if vn.successors[i] != nil {
			successors = i + 1
		}
	}
	return
}

/*
Vnode RPC implementation for localNode
*/
func (vn *localVnode) RLock(key string, nodeID string, remoteAddr string, opsLogEntry *OpsLogEntry) (string, uint, uint64, error) {
	//  TODO : Do exactly this on the TCP server implementation using the Vnode vn. Get the LM instance from the localVnode and call createRLock
	lockID, version, commitPoint, err := vn.lm.createRLock(key, nodeID, remoteAddr, opsLogEntry)
	return lockID, version, commitPoint, err
}

func (vn *localVnode) WLock(key string, version uint, timeout uint, nodeID string, opsLogEntry *OpsLogEntry) (string, uint, uint, uint64, error) {
	lockID, version, timeout, cp, err := vn.lm.createWLock(key, version, timeout, nodeID, opsLogEntry)
	return lockID, version, timeout, cp, err
}

func (vn *localVnode) CommitWLock(key string, version uint, nodeID string, opsLogEntry *OpsLogEntry) (uint64, error) {
	cp, err := vn.lm.commitWLock(key, version, nodeID, opsLogEntry)
	return cp, err
}

func (vn *localVnode) CheckWLock(key string) (bool, uint, error) {
	return vn.lm.checkWLock(key)
}

func (vn *localVnode) GetId() (string, error) {
	return vn.String(), nil // Does it even have an error component? Having it for consistent interface methods
}

func (vn *localVnode) InvalidateRLock(lockID string) error {
	lmClient := vn.lm_client
	if lmClient == nil {
		return fmt.Errorf("Client doesn't have a local LManagerClient associated with it")
	}
	err := lmClient.InvalidateRLock(lockID)
	return err
}

func (vn *localVnode) AbortWLock(key string, version uint, nodeID string, opsLogEntry *OpsLogEntry) (uint64, error) {
	cp, err := vn.lm.abortWLock(key, version, nodeID, opsLogEntry)
	return cp, err
}

func (vn *localVnode) UpdateVersionMap(versionMap *map[string]uint) {
	vn.lm.UpdateVersionMap(versionMap)
	return
}

func (vn *localVnode) Get(key string, version uint) ([]byte, error) {
	val, err := vn.store.get(key, version)

	return val, err
}

func (vn *localVnode) Set(key string, version uint, value []byte) error {
	err := vn.store.set(key, version, value)

	return err
}

func (vn *localVnode) List() ([]string, error) {
	keys, err := vn.store.list()

	return keys, err
}

func (vn *localVnode) BulkSet(key string, valLst []KVStoreValue) error {
	err := vn.store.bulkSet(key, valLst)

	return err
}

func (vn *localVnode) SyncKeys(ownerVn *Vnode, key string, ver []uint) error {
	err := vn.store.syncKeys(ownerVn, key, ver)

	return err
}

func (vn *localVnode) MissingKeys(replVn *Vnode, key string, ver []uint) error {
	err := vn.store.missingKeys(replVn, key, ver)

	return err
}

func (vn *localVnode) PurgeVersions(key string, maxVersion uint) error {
	err := vn.store.purgeVersions(key, maxVersion)

	return err
}

func (vn *localVnode) JoinRing(ringId string, self *Vnode) ([]*Vnode, error) {
	return vn.tracker.handleJoinRing(ringId, self)
}

func (vn *localVnode) LeaveRing(ringId string) error {
	panic("TODO: localVnode.LeaveRing")
}
