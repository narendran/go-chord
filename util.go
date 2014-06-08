package buddystore

import (
	"bytes"
	"fmt"
	"math/big"
	"math/rand"
	"net"
	"strconv"
	"time"

	"github.com/golang/glog"
)

// Generates a random stabilization time
func randStabilize(conf *Config) time.Duration {
	min := conf.StabilizeMin
	max := conf.StabilizeMax
	r := rand.Float64()
	return time.Duration((r * float64(max-min)) + float64(min))
}

// Checks if a key is STRICTLY between two ID's exclusively
func between(id1, id2, key []byte) bool {
	// Check for ring wrap around
	if bytes.Compare(id1, id2) == 1 {
		return bytes.Compare(id1, key) == -1 ||
			bytes.Compare(id2, key) == 1
	}

	// Handle the normal case
	return bytes.Compare(id1, key) == -1 &&
		bytes.Compare(id2, key) == 1
}

// Checks if a key is between two ID's, right inclusive
func betweenRightIncl(id1, id2, key []byte) bool {
	// Check for ring wrap around
	if bytes.Compare(id1, id2) == 1 {
		return bytes.Compare(id1, key) == -1 ||
			bytes.Compare(id2, key) >= 0
	}

	return bytes.Compare(id1, key) == -1 &&
		bytes.Compare(id2, key) >= 0
}

// Computes the offset by (n + 2^exp) % (2^mod)
func powerOffset(id []byte, exp int, mod int) []byte {
	// Copy the existing slice
	off := make([]byte, len(id))
	copy(off, id)

	// Convert the ID to a bigint
	idInt := big.Int{}
	idInt.SetBytes(id)

	// Get the offset
	two := big.NewInt(2)
	offset := big.Int{}
	offset.Exp(two, big.NewInt(int64(exp)), nil)

	// Sum
	sum := big.Int{}
	sum.Add(&idInt, &offset)

	// Get the ceiling
	ceil := big.Int{}
	ceil.Exp(two, big.NewInt(int64(mod)), nil)

	// Apply the mod
	idInt.Mod(&sum, &ceil)

	// Add together
	return idInt.Bytes()
}

// max returns the max of two ints
func max(a, b int) int {
	if a >= b {
		return a
	} else {
		return b
	}
}

// min returns the min of two ints
func min(a, b int) int {
	if a <= b {
		return a
	} else {
		return b
	}
}

// Returns the vnode nearest a key
func nearestVnodeToKey(vnodes []*Vnode, key []byte) *Vnode {
	for i := len(vnodes) - 1; i >= 0; i-- {
		if bytes.Compare(vnodes[i].Id, key) == -1 {
			return vnodes[i]
		}
	}
	// Return the last vnode
	return vnodes[len(vnodes)-1]
}

// Merges errors together
func mergeErrors(err1, err2 error) error {
	if err1 == nil {
		return err2
	} else if err2 == nil {
		return err1
	} else {
		return fmt.Errorf("%s\n%s", err1, err2)
	}
}

func printLogs(opsLog []*OpsLogEntry) {
	fmt.Println("*** LOCK OPERATIONS LOGS ***")
	for i := range opsLog {
		fmt.Println(opsLog[i].OpNum, " | ", opsLog[i].Op, " | ", opsLog[i].Key, " - ", opsLog[i].Version, " | ", opsLog[i].Timeout)
	}
	fmt.Println()
}

func CreateNewTCPTransport() (Transport, *Config) {
	port := int(rand.Uint32()%(64512) + 1024)
	glog.Infof("PORT: %d", port)

	listen := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
	glog.Infof("Listen Address: %s", listen)

	transport, err := InitTCPTransport(listen, LISTEN_TIMEOUT)
	if err != nil {
		// TODO: What if transport fails?
		// Most likely cause: listen port conflict
		panic("TODO: Is another process listening on this port?")
	}

	conf := DefaultConfig(listen)

	return transport, conf
}

// IntHeap lifted from http://golang.org/pkg/container/heap/
type IntHeap []int

func (h IntHeap) Len() int {
	return len(h)
}

func (h IntHeap) Less(i, j int) bool {
	return h[i] < h[j]
}

func (h IntHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *IntHeap) Push(x interface{}) {
	// Push and Pop use pointer receivers because they modify the slice's length,
	// not just its contents.
	*h = append(*h, x.(int))
}

func (h *IntHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}
