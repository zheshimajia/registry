// Package store provides a bolt distributed key-value store. The keys and
// associated values are changed via distributed consensus, meaning that the
// values are changed only when a majority of nodes in the cluster agree on
// the new value.
//
// Distributed consensus is provided via the Raft algorithm.
package store

import (
	"bytes"
	//"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	stdlog "log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lodastack/log"
	"github.com/lodastack/registry/common"
	"github.com/lodastack/registry/model"

	"github.com/boltdb/bolt"
	"github.com/hashicorp/raft"
	"github.com/hashicorp/raft-boltdb"
)

var bucketNotFound = errors.New("bucket not found")
var ErrNotLeader = raft.ErrNotLeader

const (
	retainSnapshotCount = 2
	raftTimeout         = 10 * time.Second
	leaderWaitDelay     = 100 * time.Millisecond
	heartbeatTimeout    = 1 * time.Second
	waitSnapshotTimeout = 60 * time.Second

	boltFile = "registry.db"
	raftDir  = "raft"

	// cacheMaxMemorySize is the maximum size
	cacheMaxMemorySize = 1024 * 1024 * 50
)

type commandType int

const (
	update                 commandType = iota // Commands which query the database.
	batch                                     // Commands which modify the database.
	createBucket                              // Commands which create the bucket.
	removeBucket                              // Commands which remove the bucket.
	removeKey                                 // Commands which remove the key.
	createBucketIfNotExist                    // Commands which create bucket if it not exist.
	setSession                                // Commands which set a session.
	delSession                                // Commands which delete a session key.

	setPeer // Command which node join.
	restore
)

// ClusterState defines the possible Raft states the current node can be in
type ClusterState int

// Represents the Raft cluster states
const (
	Leader ClusterState = iota
	Follower
	Candidate
	Shutdown
	Unknown
)

type command struct {
	Typ commandType     `json:"typ,omitempty"`
	Sub json.RawMessage `json:"sub,omitempty"`
}

func newCommand(t commandType, d interface{}) (*command, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	return &command{
		Typ: t,
		Sub: b,
	}, nil

}

type databaseSub struct {
	Name  []byte      `json:"name,omitempty"`  // bucket name for bucket management
	Batch []model.Row `json:"batch,omitempty"` // for batch update
}

// peersSub is a command which sets the API address for a Raft address.
type peersSub map[string]string

// sessionSub is a command which sets key and value for the session.
type sessionSub struct {
	Key   interface{} `json:"key,omitempty"`
	Value interface{} `json:"value,omitempty"`
}

// Transport is the interface the network service must provide.
type Transport interface {
	net.Listener

	// Dial is used to create a new outgoing connection
	Dial(address string, timeout time.Duration) (net.Conn, error)
}

// Store is a bolt key-value store, where all changes are made via Raft consensus.
type Store struct {
	Dir      string
	raftBind string
	dbPath   string
	ready    chan struct{} // Wait for snapshot

	mu sync.Mutex
	db *bolt.DB // The backend bolt store for the system.

	cache   *Cache
	session *LodaSession

	raft          *raft.Raft // The consensus mechanism
	peerStore     raft.PeerStore
	raftTransport Transport

	metaMu sync.RWMutex
	meta   *clusterMeta

	// TODO: maybe need to config
	SnapshotThreshold uint64
	HeartbeatTimeout  time.Duration

	logger *log.Logger
}

// New returns a new Store.
func New(path string, tn Transport) *Store {
	return &Store{
		Dir:              path,
		raftBind:         tn.Addr().String(),
		raftTransport:    tn,
		HeartbeatTimeout: heartbeatTimeout,
		meta:             newClusterMeta(),
		dbPath:           filepath.Join(path, boltFile),
		cache:            NewCache(cacheMaxMemorySize, nil),
		session:          NewSession(),
		logger:           log.New("INFO", "store", model.LogBackend),
	}
}

// raftConfig returns a new Raft config for the store.
func (s *Store) raftConfig() *raft.Config {
	config := raft.DefaultConfig()
	if s.SnapshotThreshold != 0 {
		config.SnapshotThreshold = s.SnapshotThreshold
	}
	if s.HeartbeatTimeout != 0 {
		config.HeartbeatTimeout = s.HeartbeatTimeout
	}
	// avoid raft logs increase fast
	config.TrailingLogs = 1000
	config.SnapshotThreshold = 500
	config.ShutdownOnRemove = false
	return config
}

// Open opens the store. If enableSingle is set, and there are no existing peers,
// then this node becomes the first node, and therefore leader, of the cluster.
func (s *Store) Open(enableSingle bool) error {
	raftPath := filepath.Join(s.Dir, raftDir)
	if err := os.MkdirAll(raftPath, 0700); err != nil {
		return err
	}

	// Open backend storage
	db, err := bolt.Open(s.dbPath, 0600, nil)
	if err != nil {
		return err
	}
	s.db = db

	// Setup Raft configuration.
	config := s.raftConfig()
	config.Logger = stdlog.New(os.Stderr, "raft", stdlog.Lshortfile)

	// Check for any existing peers.
	peers, err := readPeersJSON(filepath.Join(raftPath, "peers.json"))
	if err != nil {
		return err
	}

	// Allow the node to entry single-mode, potentially electing itself, if
	// explicitly enabled and there is only 1 node in the cluster already.
	if enableSingle && len(peers) <= 1 {
		s.logger.Println("enabling single-node mode")
		config.EnableSingleNode = true
		config.DisableBootstrapAfterElect = false
	}

	// Setup Raft communication.
	transport := raft.NewNetworkTransport(s.raftTransport, 3, 10*time.Second, os.Stderr)

	// Create peer storage.
	s.peerStore = raft.NewJSONPeers(raftPath, transport)

	// Create the snapshot store. This allows the Raft to truncate the log.
	snapshots, err := raft.NewFileSnapshotStore(raftPath, retainSnapshotCount, os.Stderr)
	if err != nil {
		return fmt.Errorf("file snapshot store: %s", err)
	}

	// Create the log store and stable store.
	logStore, err := raftboltdb.NewBoltStore(filepath.Join(raftPath, "raft.db"))
	if err != nil {
		return fmt.Errorf("new bolt store: %s", err)
	}

	// Instantiate the Raft systems.
	ra, err := raft.NewRaft(config, (*fsm)(s), logStore, logStore, snapshots, s.peerStore, transport)
	if err != nil {
		return fmt.Errorf("new raft: %s", err)
	}
	s.raft = ra
	s.cache.Open()
	s.logger.Printf("open store finished")
	return nil
}

// Close closes the store. If wait is true, waits for a graceful shutdown.
func (s *Store) Close(wait bool) error {
	if err := s.db.Close(); err != nil {
		return err
	}
	f := s.raft.Shutdown()
	if wait {
		if e := f.(raft.Future); e.Error() != nil {
			return e.Error()
		}
	}
	s.logger.Printf("store closed")
	return nil
}

// IsLeader is used to determine if the current node is cluster leader.
func (s *Store) IsLeader() bool {
	return s.raft.State() == raft.Leader
}

// Path returns the path to the store's storage directory.
func (s *Store) Path() string {
	return s.Dir
}

// Leader returns the current leader. Returns a blank string if there is
// no leader.
func (s *Store) Leader() string {
	return s.raft.Leader()
}

// Nodes returns the list of current peers.
func (s *Store) Nodes() ([]string, error) {
	return s.peerStore.Peers()
}

// Addr returns the address of the store.
func (s *Store) Addr() string {
	return s.raftTransport.Addr().String()
}

// Peer returns the API address for the given addr. If there is no peer
// for the address, it returns the empty string.
func (s *Store) Peer(addr string) string {
	return s.meta.AddrForPeer(addr)
}

// APIPeers return the map of Raft addresses to API addresses.
// Delete apiPeer record not in the cluster.
func (s *Store) APIPeers() (map[string]string, error) {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	raftPeers, err := s.peerStore.Peers()
	if err != nil {
		return nil, err
	}

	apiPeers := make(map[string]string, len(s.meta.APIPeers))
	for k, v := range s.meta.APIPeers {
		if !common.ContainsString(raftPeers, k) {
			delete(s.meta.APIPeers, k)
			continue
		}
		apiPeers[k] = v
	}
	return apiPeers, nil
}

// State returns the current node's Raft state.
func (s *Store) State() ClusterState {
	state := s.raft.State()
	switch state {
	case raft.Leader:
		return Leader
	case raft.Candidate:
		return Candidate
	case raft.Follower:
		return Follower
	case raft.Shutdown:
		return Shutdown
	default:
		return Unknown
	}
}

// WaitForLeader blocks until a leader is detected, or the timeout expires.
func (s *Store) WaitForLeader(timeout time.Duration) (string, error) {
	tck := time.NewTicker(leaderWaitDelay)
	defer tck.Stop()
	tmr := time.NewTimer(timeout)
	defer tmr.Stop()
	tms := time.NewTimer(waitSnapshotTimeout)
	defer tms.Stop()

	var leader string
	var err error

	for {
		select {
		case <-tck.C:
			l := s.Leader()
			if l != "" {
				leader = l
				err = nil
				goto WAITEND
			}
		case <-tmr.C:
			return "", fmt.Errorf("wait for leader timeout")
		}
	}

WAITEND:

	// wait for snapshot
	time.Sleep(1 * time.Second)
	if s.ready != nil {
		for {
			select {
			case <-s.ready:
				return leader, err
			case <-tms.C:
				return "", fmt.Errorf("wait for snapshot timeout")
			}
		}
	}
	return leader, err
}

// View returns the value for the given key.
func (s *Store) View(bucket, key []byte) ([]byte, error) {
	var value []byte
	if v, exist := s.cache.Get(bucket, key); exist {
		return v, nil
	}

	err := s.db.View(
		func(tx *bolt.Tx) error {
			b := tx.Bucket(bucket)
			if b == nil {
				return bucketNotFound
			}
			value = b.Get(key)
			return nil
		})
	// if the key not exist, bolt will return nil.
	if value != nil {
		s.cache.Add(bucket, key, value)
	}
	return value, err
}

// Update the value for the given key.
func (s *Store) Update(bucket []byte, key []byte, value []byte) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}

	rows := []model.Row{
		{
			Bucket: bucket,
			Key:    key,
			Value:  value,
		}}

	d := &databaseSub{
		Batch: rows,
	}

	c, err := newCommand(update, d)
	if err != nil {
		return err
	}

	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	f := s.raft.Apply(b, raftTimeout)
	if e := f.(raft.Future); e.Error() != nil {
		if e.Error() == raft.ErrNotLeader {
			return ErrNotLeader
		}
		return e.Error()
	}
	r := f.Response().(*fsmGenericResponse)
	return r.error
}

// View bucket by keyPerfix.
func (s *Store) ViewPrefix(bucket, keyPrefix []byte) (map[string]string, error) {
	var result map[string]string = make(map[string]string, 0)
	tx, err := s.db.Begin(true)
	if err != nil {
		s.logger.Error("begin db fail: ", err.Error())
		return result, err
	}
	defer tx.Rollback()

	b := tx.Bucket(bucket)
	if b == nil {
		s.logger.Error("failed to copen bucket: ", string(bucket))
		return result, bucketNotFound
	}
	c := b.Cursor()
	for k, v := c.Seek(keyPrefix); len(k) != 0 && strings.HasPrefix(string(k), string(keyPrefix)); k, v = c.Next() {
		if len(v) != 0 {
			result[string(k)] = string(v)
		}
	}
	return result, nil
}

// Batch update the values for the given keys.
func (s *Store) Batch(rows []model.Row) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}

	if len(rows) == 0 {
		return fmt.Errorf("no data in batch")
	}

	d := &databaseSub{
		Batch: rows,
	}

	c, err := newCommand(batch, d)
	if err != nil {
		return err
	}

	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	f := s.raft.Apply(b, raftTimeout)
	if e := f.(raft.Future); e.Error() != nil {
		if e.Error() == raft.ErrNotLeader {
			return ErrNotLeader
		}
		return e.Error()
	}
	r := f.Response().(*fsmGenericResponse)
	return r.error
}

// CreateBucket create a bucket.
func (s *Store) CreateBucket(name []byte) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}

	d := &databaseSub{
		Name: name,
	}

	c, err := newCommand(createBucket, d)
	if err != nil {
		return err
	}

	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	f := s.raft.Apply(b, raftTimeout)
	if e := f.(raft.Future); e.Error() != nil {
		if e.Error() == raft.ErrNotLeader {
			return ErrNotLeader
		}
		return e.Error()
	}
	r := f.Response().(*fsmGenericResponse)
	return r.error
}

// Create a bucket if not exist.
func (s *Store) CreateBucketIfNotExist(name []byte) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}

	d := &databaseSub{
		Name: name,
	}

	c, err := newCommand(createBucketIfNotExist, d)
	if err != nil {
		return err
	}

	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	f := s.raft.Apply(b, raftTimeout)
	if e := f.(raft.Future); e.Error() != nil {
		if e.Error() == raft.ErrNotLeader {
			return ErrNotLeader
		}
		return e.Error()
	}
	r := f.Response().(*fsmGenericResponse)
	return r.error
}

func (s *Store) RemoveKey(bucket, key []byte) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}

	rows := []model.Row{
		{
			Bucket: bucket,
			Key:    key,
		}}

	d := &databaseSub{
		Batch: rows,
	}

	c, err := newCommand(removeKey, d)
	if err != nil {
		return err
	}

	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	f := s.raft.Apply(b, raftTimeout)
	if e := f.(raft.Future); e.Error() != nil {
		if e.Error() == raft.ErrNotLeader {
			return ErrNotLeader
		}
		return e.Error()
	}
	r := f.Response().(*fsmGenericResponse)
	return r.error
}

// RemoveBucket remove a bucket.
func (s *Store) RemoveBucket(name []byte) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}

	d := &databaseSub{
		Name: name,
	}

	c, err := newCommand(removeBucket, d)
	if err != nil {
		return err
	}

	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	f := s.raft.Apply(b, raftTimeout)
	if e := f.(raft.Future); e.Error() != nil {
		if e.Error() == raft.ErrNotLeader {
			return ErrNotLeader
		}
		return e.Error()
	}
	r := f.Response().(*fsmGenericResponse)
	return r.error
}

// GetSession get a session.
func (s *Store) GetSession(k interface{}) interface{} {
	v := s.session.Get(k)
	if v == nil {
		//consistency latency handler here.
		return s.session.Get(k)
	}
	return v
}

// SetSession set a session.
func (s *Store) SetSession(k, v interface{}) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}

	d := &sessionSub{
		Key:   k,
		Value: v,
	}

	c, err := newCommand(setSession, d)
	if err != nil {
		return err
	}

	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	f := s.raft.Apply(b, raftTimeout)
	if e := f.(raft.Future); e.Error() != nil {
		if e.Error() == raft.ErrNotLeader {
			return ErrNotLeader
		}
		return e.Error()
	}
	r := f.Response().(*fsmGenericResponse)
	return r.error
}

// DelSession delete a session by given key.
func (s *Store) DelSession(k interface{}) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}

	d := &sessionSub{
		Key: k,
	}

	c, err := newCommand(delSession, d)
	if err != nil {
		return err
	}

	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	f := s.raft.Apply(b, raftTimeout)
	if e := f.(raft.Future); e.Error() != nil {
		if e.Error() == raft.ErrNotLeader {
			return ErrNotLeader
		}
		return e.Error()
	}
	r := f.Response().(*fsmGenericResponse)
	return r.error
}

// Backup returns a snapshot of the store.
func (s *Store) Backup() ([]byte, error) {
	// TODO: not only leader can backup
	if s.raft.State() != raft.Leader {
		return nil, ErrNotLeader
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tmpFile, err := ioutil.TempFile("", "registry-backup-")
	if err != nil {
		return nil, err
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	tx, err := s.db.Begin(true)
	if err != nil {
		return nil, err
	}

	if err := tx.CopyFile(tmpFile.Name(), 0600); err != nil {
		tx.Rollback()
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		tx.Rollback()
		return nil, err
	}

	var data []byte
	data, err = ioutil.ReadFile(tmpFile.Name())
	if err != nil {
		return nil, err
	}

	return data, nil
}

// Restore restores backup data file.
func (s *Store) Restore(backupfile string) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}

	d := &databaseSub{
		Name: []byte(backupfile),
	}

	c, err := newCommand(restore, d)
	if err != nil {
		return err
	}

	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	f := s.raft.Apply(b, raftTimeout)
	if e := f.(raft.Future); e.Error() != nil {
		if e.Error() == raft.ErrNotLeader {
			return ErrNotLeader
		}
		return e.Error()
	}
	r := f.Response().(*fsmGenericResponse)
	return r.error
}

// Join joins a node, located at addr, to this store. The node must be ready to
// respond to Raft communications at that address.
func (s *Store) Join(addr string) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}
	s.logger.Printf("received join request for remote node as %s", addr)

	f := s.raft.AddPeer(addr)
	if f.Error() != nil {
		return f.Error()
	}
	s.logger.Printf("node at %s joined successfully", addr)
	return nil
}

// Remove removes a node from the store, specified by addr.
// NOTE: raft Bug will cause the claster cannot add peer any more.
func (s *Store) Remove(addr string) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}
	s.logger.Printf("received request to remove node %s", addr)

	f := s.raft.RemovePeer(addr)
	if f.Error() != nil {
		return f.Error()
	}
	s.logger.Printf("node %s removed successfully", addr)

	return f.Error()
}

// UpdateAPIPeers updates the cluster-wide peer information.
func (s *Store) UpdateAPIPeers(peers map[string]string) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}

	c, err := newCommand(setPeer, peers)
	if err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	f := s.raft.Apply(b, raftTimeout)
	return f.Error()
}

type fsm Store

type fsmGenericResponse struct {
	error error
}

// Apply applies a Raft log entry to the key-value store.
func (f *fsm) Apply(l *raft.Log) interface{} {
	var c command
	if err := json.Unmarshal(l.Data, &c); err != nil {
		f.logger.Printf("failed to unmarshal command: %s", err.Error())
		return &fsmGenericResponse{error: err}
	}

	switch c.Typ {
	case update:
		err := f.applyUpdate(c.Sub)
		return &fsmGenericResponse{error: err}
	case batch:
		err := f.applyBatch(c.Sub)
		return &fsmGenericResponse{error: err}
	case createBucket:
		err := f.applyCreateBucket(c.Sub)
		return &fsmGenericResponse{error: err}
	case removeKey:
		err := f.applyRemoveKey(c.Sub)
		return &fsmGenericResponse{error: err}
	case removeBucket:
		err := f.applyRemoveBucket(c.Sub)
		return &fsmGenericResponse{error: err}
	case createBucketIfNotExist:
		err := f.applyCreateBucketIfNotExist(c.Sub)
		return &fsmGenericResponse{error: err}
	case setSession:
		err := f.applySetSession(c.Sub)
		return &fsmGenericResponse{error: err}
	case delSession:
		err := f.applyDelSession(c.Sub)
		return &fsmGenericResponse{error: err}
	case setPeer:
		err := f.applySetPeer(c.Sub)
		return &fsmGenericResponse{error: err}
	case restore:
		err := f.applyRestore(c.Sub)
		return &fsmGenericResponse{error: err}
	default:
		err := fmt.Errorf("unrecognized command op: %s", c.Typ)
		f.logger.Printf(err.Error())
		return &fsmGenericResponse{error: err}
	}
}

// Snapshot returns a snapshot of the key-value store.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	snapFile, err := ioutil.TempFile("", "registry-snap-")
	if err != nil {
		return nil, err
	}
	snapFile.Close()
	defer os.Remove(snapFile.Name())

	tx, err := f.db.Begin(true)
	if err != nil {
		return nil, err
	}

	if err := tx.CopyFile(snapFile.Name(), 0600); err != nil {
		tx.Rollback()
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		tx.Rollback()
		return nil, err
	}

	fsm := &fsmSnapshot{}
	fsm.database, err = ioutil.ReadFile(snapFile.Name())
	if err != nil {
		log.Printf("Failed to read database for snapshot: %s", err.Error())
		return nil, err
	}

	return fsm, nil
}

// Restore stores the key-value store to a previous state.
func (f *fsm) Restore(rc io.ReadCloser) error {
	f.ready = make(chan struct{})
	defer func() {
		close(f.ready)
		f.ready = nil
	}()
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.db.Close(); err != nil {
		return err
	}

	var database []byte
	if err := json.NewDecoder(rc).Decode(&database); err != nil {
		return err
	}

	var db *bolt.DB
	var err error

	// Write snapshot over any existing database file.
	if err := ioutil.WriteFile(f.dbPath, database, 0660); err != nil {
		return err
	}

	// Re-open it.
	// Open backend storage
	db, err = bolt.Open(f.dbPath, 0600, nil)
	if err != nil {
		return err
	}

	f.db = db
	return nil
}

func (f *fsm) applySetPeer(sub json.RawMessage) error {
	var d peersSub
	if err := json.Unmarshal(sub, &d); err != nil {
		return err
	}

	f.metaMu.Lock()
	defer f.metaMu.Unlock()
	for k, v := range d {
		f.meta.APIPeers[k] = v
	}

	return nil
}

func (f *fsm) applyUpdate(sub json.RawMessage) error {
	var d databaseSub
	if err := json.Unmarshal(sub, &d); err != nil {
		return err
	}
	rows := d.Batch

	if len(rows) != 1 {
		return fmt.Errorf("update just accept 1 row data: %d", len(rows))
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(rows[0].Bucket)
		if b == nil {
			return bucketNotFound
		}
		err := b.Put(rows[0].Key, rows[0].Value)

		// remove cache
		f.cache.Remove(rows[0].Bucket, rows[0].Key)
		return err
	})
}

func (f *fsm) applyRemoveKey(sub json.RawMessage) error {
	var d databaseSub
	if err := json.Unmarshal(sub, &d); err != nil {
		return err
	}
	rows := d.Batch

	if len(rows) != 1 {
		return fmt.Errorf("delete key just accept 1 row data: %d", len(rows))
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(rows[0].Bucket)
		if b == nil {
			return bucketNotFound
		}
		err := b.Delete(rows[0].Key)

		// remove cache
		f.cache.Remove(rows[0].Bucket, rows[0].Key)
		return err
	})
}

func (f *fsm) applyBatch(sub json.RawMessage) error {
	var d databaseSub
	if err := json.Unmarshal(sub, &d); err != nil {
		return err
	}
	rows := d.Batch

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.db.Batch(func(tx *bolt.Tx) error {
		for _, row := range rows {
			b := tx.Bucket(row.Bucket)
			if b == nil {
				return bucketNotFound
			}
			if err := b.Put(row.Key, row.Value); err != nil {
				return err
			}
			// remove cache
			f.cache.Remove(row.Bucket, row.Key)
		}
		return nil
	})
}

func (f *fsm) applyCreateBucket(sub json.RawMessage) error {
	var d databaseSub
	if err := json.Unmarshal(sub, &d); err != nil {
		return err
	}
	name := d.Name

	f.mu.Lock()
	defer f.mu.Unlock()

	// remove cache at first
	f.cache.RemoveBucket(name)

	return f.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucket(name)
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}
		return nil
	})
}

func (f *fsm) applyCreateBucketIfNotExist(sub json.RawMessage) error {
	var d databaseSub
	if err := json.Unmarshal(sub, &d); err != nil {
		return err
	}
	name := d.Name

	f.mu.Lock()
	defer f.mu.Unlock()

	// remove cache at first
	f.cache.RemoveBucket(name)

	return f.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(name)
		if err != nil {
			return fmt.Errorf("create bucket if not exist: %s", err)
		}
		return nil
	})
}

func (f *fsm) applyRemoveBucket(sub json.RawMessage) error {
	var d databaseSub
	if err := json.Unmarshal(sub, &d); err != nil {
		return err
	}
	name := d.Name

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.db.Update(func(tx *bolt.Tx) error {
		err := tx.DeleteBucket(name)
		if err != nil {
			return fmt.Errorf("remove bucket: %s - %s", err, string(name))
		}
		// remove cache at last
		f.cache.RemoveBucket(name)
		return nil
	})
}

func (f *fsm) applySetSession(sub json.RawMessage) error {
	var d sessionSub
	if err := json.Unmarshal(sub, &d); err != nil {
		return err
	}

	f.session.Set(d.Key, d.Value)
	return nil
}

func (f *fsm) applyDelSession(sub json.RawMessage) error {
	var d sessionSub
	if err := json.Unmarshal(sub, &d); err != nil {
		return err
	}

	f.session.Delete(d.Key)
	return nil
}

// Restore stores the key-value store to a backup data file.
func (f *fsm) applyRestore(sub json.RawMessage) error {
	var d databaseSub
	if err := json.Unmarshal(sub, &d); err != nil {
		return err
	}
	file := string(d.Name)

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.db.Close(); err != nil {
		return err
	}

	defer func() {
		// Re-open it.
		// Open backend storage
		db, err := bolt.Open(f.dbPath, 0600, nil)
		if err != nil {
			panic(err)
		}
		f.cache.Purge()
		f.db = db
	}()

	// start restore data file
	backup, err := os.Open(file)
	if err != nil {
		return err
	}
	defer backup.Close()

	dbfile, err := os.OpenFile(f.dbPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer dbfile.Close()

	// Write backup data file over any existing database file.
	// buffer: 32MB
	if _, err := io.Copy(dbfile, backup); err != nil {
		return err
	}

	return nil
}

type fsmSnapshot struct {
	database []byte
}

func (f *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	err := func() error {
		// Encode data.
		// TODO: use binary to encode.
		b, err := json.Marshal(f.database)
		if err != nil {
			return err
		}

		// Write data to sink.
		if _, err := sink.Write(b); err != nil {
			return err
		}

		// Close the sink.
		if err := sink.Close(); err != nil {
			return err
		}

		return nil
	}()

	if err != nil {
		sink.Cancel()
		return err
	}

	return nil
}

func (f *fsmSnapshot) Release() {}

func readPeersJSON(path string) ([]string, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	if len(b) == 0 {
		return nil, nil
	}

	var peers []string
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&peers); err != nil {
		return nil, err
	}

	return peers, nil
}
