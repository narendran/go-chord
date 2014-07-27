/*
This package is used to provide an implementation of the
Chord network protocol.
*/
package buddystore

import (
	"crypto/sha1"
	"fmt"
	"hash"
	"sync"
	"time"

	"github.com/golang/glog"
)

const JOIN_STABILIZE_WAIT = 5

// Implements the methods needed for a Chord ring
type Transport interface {
	// Gets a list of the vnodes on the box
	ListVnodes(string) ([]*Vnode, error)

	// Ping a Vnode, check for liveness
	Ping(*Vnode) (bool, error)

	// Request a nodes predecessor
	GetPredecessor(*Vnode) (*Vnode, error)

	// Notify our successor of ourselves
	Notify(target, self *Vnode) ([]*Vnode, error)

	// Find a successor
	FindSuccessors(*Vnode, int, []byte) ([]*Vnode, error)

	// Clears a predecessor if it matches a given vnode. Used to leave.
	ClearPredecessor(target, self *Vnode) error

	// Instructs a node to skip a given successor. Used to leave.
	SkipSuccessor(target, self *Vnode) error

	// Get the list of predecessors
	GetPredecessorList(*Vnode) ([]*Vnode, error)

	// Register for an RPC callbacks
	Register(*Vnode, VnodeRPC)

	// Lock Manager operations
	RLock(*Vnode, string, string, *OpsLogEntry) (string, uint, uint64, error)
	WLock(*Vnode, string, uint, uint, string, *OpsLogEntry) (string, uint, uint, uint64, error)
	CommitWLock(*Vnode, string, uint, string, *OpsLogEntry) (uint64, error)
	AbortWLock(*Vnode, string, uint, string, *OpsLogEntry) (uint64, error)
	InvalidateRLock(*Vnode, string) error

	// KV Store operations
	Get(target *Vnode, key string, version uint) ([]byte, error)
	Set(target *Vnode, key string, version uint, value []byte) error
	List(target *Vnode) ([]string, error)
	BulkSet(target *Vnode, key string, valLst []KVStoreValue) error
	SyncKeys(target *Vnode, ownerVn *Vnode, key string, ver []uint) error
	MissingKeys(target *Vnode, replVn *Vnode, key string, ver []uint) error
	PurgeVersions(target *Vnode, key string, maxVersion uint) error

	// Tracker operations
	JoinRing(target *Vnode, ringId string, self *Vnode) ([]*Vnode, error)
	LeaveRing(target *Vnode, ringId string) error

	// TODO: Is this the right place?
	IsLocalVnode(vn *Vnode) bool
}

// These are the methods to invoke on the registered vnodes
type VnodeRPC interface {
	GetPredecessor() (*Vnode, error)
	Notify(*Vnode) ([]*Vnode, error)
	FindSuccessors(int, []byte) ([]*Vnode, error)
	ClearPredecessor(*Vnode) error
	SkipSuccessor(*Vnode) error
	GetPredecessorList() ([]*Vnode, error)
	GetId() (string, error)

	// KV Store operations
	Get(key string, version uint) ([]byte, error)
	Set(key string, version uint, value []byte) error
	List() ([]string, error)
	BulkSet(key string, valLst []KVStoreValue) error
	SyncKeys(ownerVn *Vnode, key string, ver []uint) error
	MissingKeys(replVn *Vnode, key string, ver []uint) error
	PurgeVersions(key string, maxVersion uint) error

	// Lock Manager operations
	RLock(key string, nodeID string, remoteAddr string, opsLogEntry *OpsLogEntry) (string, uint, uint64, error)
	WLock(key string, version uint, timeout uint, nodeID string, opsLogEntry *OpsLogEntry) (string, uint, uint, uint64, error)
	CommitWLock(key string, version uint, nodeID string, opsLogEntry *OpsLogEntry) (uint64, error)
	AbortWLock(key string, version uint, nodeID string, opsLogEntry *OpsLogEntry) (uint64, error)
	InvalidateRLock(lockID string) error
	CheckWLock(key string) (bool, uint, error)
	UpdateVersionMap(versionMap *map[string]uint)

	// Tracker operations
	JoinRing(ringId string, self *Vnode) ([]*Vnode, error)
	LeaveRing(ringId string) error
}

// Delegate to notify on ring events
type Delegate interface {
	NewPredecessor(local, remoteNew, remotePrev *Vnode)
	Leaving(local, pred, succ *Vnode)
	PredecessorLeaving(local, remote *Vnode)
	SuccessorLeaving(local, remote *Vnode)
	Shutdown()
}

// Configuration for Chord nodes
type Config struct {
	Hostname      string           // Local host name
	NumVnodes     int              // Number of vnodes per physical node
	HashFunc      func() hash.Hash // Hash function to use
	StabilizeMin  time.Duration    // Minimum stabilization time
	StabilizeMax  time.Duration    // Maximum stabilization time
	NumSuccessors int              // Number of successors to maintain
	Delegate      Delegate         // Invoked to handle ring events
	hashBits      int              // Bit size of the hash function
	RingId        string
}

// Represents an Vnode, local or remote
type Vnode struct {
	Id   []byte // Virtual ID
	Host string // Host identifier
}

type localVnodeIface interface {
	Ring() RingIntf
	Successors() []*Vnode
	Predecessors() []*Vnode
	Predecessor() *Vnode
	localVnodeId() []byte
	GetVnode() *Vnode
}

// Represents a local Vnode
type localVnode struct {
	Vnode
	ring         *Ring
	successors   []*Vnode
	predecessors []*Vnode
	finger       []*Vnode
	last_finger  int
	predecessor  *Vnode
	stabilized   time.Time
	timer        *time.Timer
	store        *KVStore
	lm           *LManager
	lm_client    *LManagerClient
	tracker      Tracker

	// Implements:
	localVnodeIface
}

func (lvn *localVnode) Ring() RingIntf {
	return lvn.ring
}
func (lvn *localVnode) Successors() []*Vnode {
	return lvn.successors
}
func (lvn *localVnode) Predecessors() []*Vnode {
	return lvn.predecessors
}
func (lvn *localVnode) Predecessor() *Vnode {
	return lvn.predecessor
}
func (lvn *localVnode) localVnodeId() []byte {
	return lvn.Id
}
func (lvn *localVnode) GetVnode() *Vnode {
	return &lvn.Vnode
}

type RingIntf interface {
	Leave() error
	Shutdown()
	Lookup(n int, key []byte) ([]*Vnode, error)
	Transport() Transport
	GetNumSuccessors() int
	GetLocalVnode() *Vnode
	GetLocalLocalVnode() *localVnode
	GetRingId() string
	GetHashFunc() func() hash.Hash
}

// Stores the state required for a Chord ring
type Ring struct {
	config            *Config
	transport         Transport
	vnodes            []*localVnode
	delegateCh        chan func()
	shutdownComplete  chan bool
	shutdownRequested bool
	shutdownLock      sync.Mutex

	// Implements:
	RingIntf
}

// Returns the default Ring configuration
func DefaultConfig(hostname string) *Config {
	return &Config{
		hostname,
		8,        // 8 vnodes
		sha1.New, // SHA1
		time.Duration(5 * time.Second),
		time.Duration(15 * time.Second),
		8,   // 8 successors
		nil, // No delegate
		160, // 160bit hash function
		"",
	}
}

// Creates a new Chord ring given the config and transport
func Create(conf *Config, trans Transport) (*Ring, error) {
	// Initialize the hash bits
	conf.hashBits = conf.HashFunc().Size() * 8

	// Create and initialize a ring
	ring := &Ring{}
	ring.init(conf, trans)
	ring.setLocalSuccessors()
	ring.setLocalPredecessors()
	ring.schedule()
	return ring, nil
}

// Joins an existing Chord ring
func Join(conf *Config, trans Transport, existing string) (*Ring, error) {
	// Initialize the hash bits
	conf.hashBits = conf.HashFunc().Size() * 8

	// Request a list of Vnodes from the remote host
	hosts, err := trans.ListVnodes(existing)
	if err != nil {
		return nil, err
	}
	if hosts == nil || len(hosts) == 0 {
		return nil, fmt.Errorf("Remote host has no vnodes!")
	}

	if glog.V(2) {
		glog.Infof("Fetched hosts: %s", hosts)
	}

	// Create a ring
	ring := &Ring{}
	ring.init(conf, trans)

	// Acquire a live successor for each Vnode
	for _, vn := range ring.vnodes {
		// Get the nearest remote vnode
		nearest := nearestVnodeToKey(hosts, vn.Id)

		// Query for a list of successors to this Vnode
		succs, err := trans.FindSuccessors(nearest, conf.NumSuccessors, vn.Id)
		if err != nil {
			return nil, fmt.Errorf("Failed to find successor for vnodes! Got %s", err)
		}
		if succs == nil || len(succs) == 0 {
			return nil, fmt.Errorf("Failed to find successor for vnodes! Got no vnodes!")
		}

		// Assign the successors
		for idx, s := range succs {
			vn.successors[idx] = s
		}
	}

	// Do a fast stabilization, will schedule regular execution
	for _, vn := range ring.vnodes {
		vn.stabilize()
	}
	return ring, nil
}

/* BlockingJoin. Called by the buddynode that wants to block all operations until the network is healed.
Reason : All its operations should happen in its namespace. And its namespace i.e. the ring, specicifically the bootstrap members are present in the original ring
*/
func BlockingJoin(conf *Config, trans Transport, existing string) (*Ring, error) {
	// Initialize the hash bits
	conf.hashBits = conf.HashFunc().Size() * 8

	// Request a list of Vnodes from the remote host
	hosts, err := trans.ListVnodes(existing)
	if err != nil {
		return nil, err
	}
	if hosts == nil || len(hosts) == 0 {
		return nil, fmt.Errorf("Remote host has no vnodes!")
	}

	if glog.V(2) {
		glog.Infof("Fetched hosts: %s", hosts)
	}

	// Create a ring
	ring := &Ring{}
	ring.initBlockingLM(conf, trans)

	// Acquire a live successor for each Vnode
	for _, vn := range ring.vnodes {
		// Get the nearest remote vnode
		nearest := nearestVnodeToKey(hosts, vn.Id)

		// Query for a list of successors to this Vnode
		succs, err := trans.FindSuccessors(nearest, conf.NumSuccessors, vn.Id)
		if err != nil {
			return nil, fmt.Errorf("Failed to find successor for vnodes! Got %s", err)
		}
		if succs == nil || len(succs) == 0 {
			return nil, fmt.Errorf("Failed to find successor for vnodes! Got no vnodes!")
		}

		// Assign the successors
		for idx, s := range succs {
			vn.successors[idx] = s
		}
	}

	// Do a fast stabilization, will schedule regular execution
	for _, vn := range ring.vnodes {
		vn.stabilize()
		vn.lm.cancelCheckStatus = time.AfterFunc(JOIN_STABILIZE_WAIT*time.Second, vn.lm.CheckStatus)
	}
	return ring, nil
}

// Leaves a given Chord ring and shuts down the local vnodes
func (r *Ring) Leave() error {
	// Shutdown the vnodes first to avoid further stabilization runs
	r.stopVnodes()

	// Instruct each vnode to leave
	var err error
	for _, vn := range r.vnodes {
		err = mergeErrors(err, vn.leave())
	}

	// Wait for the delegate callbacks to complete
	r.stopDelegate()
	return err
}

// Shutdown shuts down the local processes in a given Chord ring
// Blocks until all the vnodes terminate.
func (r *Ring) Shutdown() {
	r.stopVnodes()
	r.stopDelegate()
}

// Does a key lookup for up to N successors of a key
func (r *Ring) Lookup(n int, key []byte) ([]*Vnode, error) {
	// Ensure that n is sane
	if n > r.config.NumSuccessors {
		return nil, fmt.Errorf("Cannot ask for more successors than NumSuccessors!")
	}

	// Hash the key
	h := r.config.HashFunc()
	h.Write(key)
	key_hash := h.Sum(nil)

	// Find the nearest local vnode
	nearest := r.nearestVnode(key_hash)

	// Use the nearest node for the lookup
	successors, err := nearest.FindSuccessors(n, key_hash)
	if err != nil {
		return nil, err
	}

	// Trim the nil successors
	for successors[len(successors)-1] == nil {
		successors = successors[:len(successors)-1]
	}
	return successors, nil
}

func (r *Ring) Transport() Transport {
	return r.transport
}

func (r *Ring) GetNumSuccessors() int {
	return r.config.NumSuccessors
}

func (r *Ring) GetRingId() string {
	return r.config.RingId
}

func (r *Ring) GetLocalLocalVnode() *localVnode {
	return r.vnodes[0]
}

func (r *Ring) GetLocalVnode() *Vnode {
	// TODO: Questionable code
	// Is vnodes[0] always going to be a local node?
	return &r.GetLocalLocalVnode().Vnode
}

func (r *Ring) GetHashFunc() func() hash.Hash {
	return r.config.HashFunc
}

func (r *Ring) GetConfig() *Config {
	return r.config
}
