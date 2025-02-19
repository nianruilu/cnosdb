package meta

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cnosdb/cnosdb/pkg/network"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	"github.com/hashicorp/raft-boltdb"
)

// Raft configuration.
const (
	raftLogCacheSize      = 512
	raftSnapshotsRetained = 2
	raftTransportMaxPool  = 3
	raftTransportTimeout  = 10 * time.Second
)

// raftState is a consensus strategy that uses a local raft implementation for
// consensus operations.
type raftState struct {
	wg        sync.WaitGroup
	config    *ServerConfig
	closing   chan struct{}
	raft      *raft.Raft
	transport *raft.NetworkTransport
	raftStore *raftboltdb.BoltStore
	raftLayer *raftLayer
	ln        net.Listener
	addr      string
	logger    hclog.Logger
	path      string
}

func newRaftState(c *ServerConfig, addr string) *raftState {
	return &raftState{
		config: c,
		addr:   addr,
		logger: hclog.New(&hclog.LoggerOptions{
			Name:  "raft-state",
			Level: hclog.LevelFromString("INFO"),
		}),
	}
}

func (r *raftState) open(s *store, ln net.Listener) error {
	r.ln = ln
	r.closing = make(chan struct{})

	// Setup raft configuration.
	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(s.raftAddr)
	config.LogOutput = ioutil.Discard

	if r.config.ClusterTracing {
		config.Logger = r.logger
	}
	config.HeartbeatTimeout = time.Duration(r.config.HeartbeatTimeout)
	config.ElectionTimeout = time.Duration(r.config.ElectionTimeout)
	config.LeaderLeaseTimeout = time.Duration(r.config.LeaderLeaseTimeout)
	config.CommitTimeout = time.Duration(r.config.CommitTimeout)
	// Since we actually never call `removePeer` this is safe.
	// If in the future we decide to call remove peer we have to re-evaluate how to handle this
	config.ShutdownOnRemove = false

	// Build raft layer to multiplex listener.
	r.raftLayer = newRaftLayer(r.addr, r.ln)

	// Create a transport layer
	r.transport = raft.NewNetworkTransport(r.raftLayer, 3, 10*time.Second, config.LogOutput)

	// Create the log store and stable store.
	store, err := raftboltdb.NewBoltStore(filepath.Join(r.path, "raft.db"))
	if err != nil {
		return fmt.Errorf("new bolt store: %s", err)
	}
	r.raftStore = store

	// Create the snapshot store.
	snapshots, err := raft.NewFileSnapshotStore(r.path, raftSnapshotsRetained, os.Stderr)
	if err != nil {
		return fmt.Errorf("file snapshot store: %s", err)
	}

	// Create raft log.
	ra, err := raft.NewRaft(config, (*storeFSM)(s), store, store, snapshots, r.transport)
	if err != nil {
		return fmt.Errorf("new raft: %s", err)
	}
	r.raft = ra

	if configFuture := ra.GetConfiguration(); configFuture.Error() != nil {
		r.logger.Info("failed to get raft configuration", configFuture.Error())
		return configFuture.Error()
	} else {
		newConfig := configFuture.Configuration()
		if newConfig.Servers == nil || len(newConfig.Servers) == 0 {
			r.logger.Info("bootstrap needed")
			configuration := raft.Configuration{
				Servers: []raft.Server{
					{
						ID:      config.LocalID,
						Address: r.transport.LocalAddr(),
					},
				},
			}
			r.logger.Info("bootstrapping new raft cluster")
			ra.BootstrapCluster(configuration)
		} else {
			r.logger.Info("no bootstrap needed")
		}
	}

	r.wg.Add(1)
	go r.logLeaderChanges()

	return nil
}

func (r *raftState) logLeaderChanges() {
	defer r.wg.Done()
	// Logs our current state (Node at 1.2.3.4:8088 [Follower])
	r.logger.Info(r.raft.String())

	for {
		select {
		case <-r.closing:
			return
		case <-r.raft.LeaderCh():
			peers, err := r.peers()
			if err != nil {
				r.logger.Info("failed to lookup peers", "error", err)
			}
			r.logger.Info(r.raft.String(), "peers", peers)
		}
	}
}

func (r *raftState) close() error {
	if r == nil {
		return nil
	}
	if r.closing != nil {
		close(r.closing)
	}
	r.wg.Wait()

	if r.transport != nil {
		r.transport.Close()
		r.transport = nil
	}

	// Shutdown raft.
	if r.raft != nil {
		if err := r.raft.Shutdown().Error(); err != nil {
			return err
		}
		r.raft = nil
	}

	if r.raftStore != nil {
		r.raftStore.Close()
		r.raftStore = nil
	}

	return nil
}

// apply applies a serialized command to the raft log.
func (r *raftState) apply(b []byte) error {
	// Apply to raft log.
	f := r.raft.Apply(b, 0)
	if err := f.Error(); err != nil {
		return err
	}

	// Return response if it's an error.
	// No other non-nil objects should be returned.
	resp := f.Response()
	if err, ok := resp.(error); ok {
		return err
	}
	if resp != nil {
		panic(fmt.Sprintf("unexpected response: %#v", resp))
	}

	return nil
}

func (r *raftState) lastIndex() uint64 {
	return r.raft.LastIndex()
}

func (r *raftState) snapshot() error {
	future := r.raft.Snapshot()
	return future.Error()
}

// addVoter instead of addPeer, adds addr to the list of peers in the cluster.
func (r *raftState) addVoter(addr string) error {
	serverAddr := raft.ServerAddress(addr)

	var servers []raft.Server
	if configFuture := r.raft.GetConfiguration(); configFuture.Error() != nil {
		r.logger.Info("failed to get raft configuration", configFuture.Error())
		return configFuture.Error()
	} else {
		servers = configFuture.Configuration().Servers
	}

	for _, srv := range servers {
		if srv.Address == serverAddr {
			return nil
		}
	}

	if fut := r.raft.AddVoter(raft.ServerID(addr), raft.ServerAddress(addr), 0, 0); fut.Error() != nil {
		return fut.Error()
	}
	return nil
}

// removeVoter instead of removePeer removes addr from the list of peers in the cluster.
func (r *raftState) removeVoter(addr string) error {
	// Only do this on the leader
	if !r.isLeader() {
		return raft.ErrNotLeader
	}

	serverAddr := raft.ServerAddress(addr)

	var servers []raft.Server
	if cfu := r.raft.GetConfiguration(); cfu.Error() != nil {
		r.logger.Info("failed to get raft configuration", cfu.Error())
		return cfu.Error()
	} else {
		servers = cfu.Configuration().Servers
	}

	var srv raft.Server
	var exists bool
	for _, srv = range servers {
		if srv.Address == serverAddr {
			exists = true
			break
		}
	}

	if !exists {
		return nil
	}

	if fut := r.raft.RemoveServer(srv.ID, 0, 0); fut.Error() != nil {
		return fut.Error()
	}
	return nil
}

func (r *raftState) peers() ([]string, error) {

	if configFuture := r.raft.GetConfiguration(); configFuture.Error() != nil {
		r.logger.Info("failed to get raft configuration", configFuture.Error())
		return []string{}, configFuture.Error()
	} else {
		peers := []string{}
		for _, srv := range configFuture.Configuration().Servers {
			peers = append(peers, string(srv.Address))
		}
		return peers, nil
	}
}

func (r *raftState) leader() string {
	if r.raft == nil {
		return ""
	}

	return string(r.raft.Leader())
}

func (r *raftState) isLeader() bool {
	if r.raft == nil {
		return false
	}
	return r.raft.State() == raft.Leader
}

// raftLayer wraps the connection so it can be re-used for forwarding.
type raftLayer struct {
	addr   *raftLayerAddr
	ln     net.Listener
	conn   chan net.Conn
	closed chan struct{}
}

type raftLayerAddr struct {
	addr string
}

func (r *raftLayerAddr) Network() string {
	return "tcp"
}

func (r *raftLayerAddr) String() string {
	return r.addr
}

// newRaftLayer returns a new instance of raftLayer.
func newRaftLayer(addr string, ln net.Listener) *raftLayer {
	return &raftLayer{
		addr:   &raftLayerAddr{addr},
		ln:     ln,
		conn:   make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

// Addr returns the local address for the layer.
func (l *raftLayer) Addr() net.Addr {
	return l.addr
}

// Dial creates a new network connection.
func (l *raftLayer) Dial(addr raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	//return net.DialTimeout("tcp", string(addr), timeout)
	return network.DialTimeout("tcp", string(addr), RaftMuxHeader, timeout)
}

// Accept waits for the next connection.
func (l *raftLayer) Accept() (net.Conn, error) {
	return l.ln.Accept()
}

// Close closes the layer.
func (l *raftLayer) Close() error { return l.ln.Close() }
