package dht

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/neoql/btlet/tools"
	"github.com/willf/bloom"
)

// CrawCallback will call when Crawler craw a infohash
type CrawCallback func(infoHash string, peerIP net.IP, peerPort int)

// Crawler can crawl infohash from dht.
type Crawler interface {
	Crawl(ctx context.Context, callback CrawCallback) error
}

// SybilCrawler can crawl info hash from DHT.
type SybilCrawler struct {
	ip     string
	port   int
	nodeID string
}

// NewSybilCrawler returns a new Crawler instance.
func NewSybilCrawler(ip string, port int) *SybilCrawler {
	return &SybilCrawler{
		ip:     ip,
		port:   port,
		nodeID: tools.RandomString(20),
	}
}

// Crawl ovrride Crawler.Crawl
func (crawler *SybilCrawler) Crawl(ctx context.Context, callback CrawCallback) error {
	core, err := NewCore(crawler.ip, crawler.port)
	if err != nil {
		return err
	}

	handle := core.Handle(crawler.nodeID)
	dispatcher := NewTransactionDispatcher(handle)
	transaction := newSybilTransaction(tools.RandomString(2), callback)

	err = dispatcher.Add(transaction)
	if err != nil {
		return err
	}
	defer dispatcher.Remove(transaction.ID())

	disposer := &sybilMessageDisposer{
		handle:      handle,
		dispatcher:  dispatcher,
		transaction: transaction,
	}

	return core.Serv(ctx, disposer)
}

type sybilMessageDisposer struct {
	handle      Handle
	dispatcher  *TransactionDispatcher
	transaction *sybilTransaction
}

func (disposer *sybilMessageDisposer) DisposeQuery(nd *Node, transactionID string, q string, args map[string]interface{}) error {
	disposer.transaction.OnQuery(disposer.handle, nd, transactionID, q, args)
	return nil
}

func (disposer *sybilMessageDisposer) DisposeResponse(nd *Node, transactionID string, resp map[string]interface{}) error {
	disposer.dispatcher.DisposeResponse(transactionID, nd, resp)
	return nil
}

func (disposer *sybilMessageDisposer) DisposeError(transactionID string, code int, describe string) error {
	return nil
}

func (disposer *sybilMessageDisposer) DisposeUnknownMessage(y string, message map[string]interface{}) error {
	return nil
}

type sybilTransaction struct {
	id           string
	lock         sync.RWMutex
	target       string
	filter       *nodeFilter
	crawCallback CrawCallback
	finish       chan struct{}
}

func newSybilTransaction(id string, callback CrawCallback) *sybilTransaction {
	return &sybilTransaction{
		id:           tools.RandomString(2),
		target:       tools.RandomString(20),
		filter:       newNodeFilter(8 * 1024 * 1024),
		crawCallback: callback,
		finish:       make(chan struct{}),
	}
}

func (transaction *sybilTransaction) ID() string {
	return transaction.id
}

func (transaction *sybilTransaction) ShelfLife() time.Duration {
	return time.Second * 30
}

func (transaction *sybilTransaction) Target() string {
	transaction.lock.RLock()
	defer transaction.lock.RUnlock()
	return transaction.target
}

func (transaction *sybilTransaction) OnLaunch(handle Handle) {
	transaction.boot(handle, defaultBootstrap)
	go func() {
	LOOP:
		for {
			select {
			case <-time.After(time.Minute):
				if transaction.filter.IsFull() {
					transaction.changeTarget()
					transaction.filter.Reset()
				}
			case <-transaction.finish:
				break LOOP
			}
		}
	}()
}

func (transaction *sybilTransaction) boot(handle Handle, bootstrap []string) {
	nodes := make([]*Node, len(bootstrap))
	for i, url := range bootstrap {
		addr, err := net.ResolveUDPAddr("udp", url)
		if err != nil {
			// TODO: handle error
			continue
		}
		nodes[i] = &Node{addr, ""}
	}

	transaction.findTargetNode(handle, transaction.Target(), nodes...)
}

func (transaction *sybilTransaction) OnError(code int, describe string) bool {
	return true
}

func (transaction *sybilTransaction) OnResponse(handle Handle,
	nd *Node, resp map[string]interface{}) bool {

	transaction.filter.AddNode(nd)

	if nds, ok := resp["nodes"]; ok {
		nodes, err := unpackNodes(nds.(string))
		if err != nil {
			// TODO: handle error
		}

		target := transaction.Target()
		for _, nd := range nodes {
			if transaction.filter.Check(nd) {
				transaction.findTargetNode(handle, target, nd)
			}
		}
	}
	return true
}

func (transaction *sybilTransaction) OnQuery(handle Handle,
	nd *Node, transactionID string, q string, args map[string]interface{}) {

	switch q {
	case "ping":
		handle.SendMessage(nd, makeResponse(transactionID, map[string]interface{}{
			"id": makeID(nd.ID, handle.NodeID()),
		}))
	case "find_node":
	case "get_peers":
		handle.SendMessage(nd, makeResponse(transactionID, map[string]interface{}{
			"id":    makeID(nd.ID, handle.NodeID()),
			"token": tools.RandomString(20),
			"nodes": "",
		}))
	case "announce_peer":
		infoHash := args["info_hash"].(string)
		port := args["port"].(int)
		if transaction.crawCallback != nil {
			defer transaction.crawCallback(infoHash, nd.Addr.IP, port)
		}
		handle.SendMessage(nd, makeResponse(transactionID, map[string]interface{}{
			"id": makeID(nd.ID, handle.NodeID()),
		}))
	default:
	}

	if transaction.filter.Check(nd) {
		transaction.findTargetNode(handle, transaction.Target(), nd)
	}
}

func (transaction *sybilTransaction) OnTimeout(handle Handle) bool {
	defer transaction.boot(handle, defaultBootstrap)
	transaction.changeTarget()
	transaction.filter.Reset()
	return true
}

func (transaction *sybilTransaction) changeTarget() {
	transaction.lock.Lock()
	defer transaction.lock.Unlock()

	len := rand.Int() % 20
	transaction.target = transaction.target[:len] + tools.RandomString(uint(20-len))
}

func (transaction *sybilTransaction) OnFinish(handle Handle) {
	close(transaction.finish)
}

func (transaction *sybilTransaction) findTargetNode(handle Handle, target string, nodes ...*Node) {
	for _, nd := range nodes {
		msg, err := makeQuery("find_node", transaction.id, map[string]interface{}{
			"target": target,
			"id":     makeID(nd.ID, handle.NodeID()),
		})
		if err != nil {
			// TODO: handle error
			continue
		}

		handle.SendMessage(nd, msg)
	}
}

func makeID(dst string, id string) string {
	if len(dst) == 0 {
		return id
	}
	return dst[:15] + id[15:]
}

type nodeFilter struct {
	lock   sync.RWMutex
	core   *bloom.BloomFilter
	amount uint
	cap    uint
}

func newNodeFilter(cap uint) *nodeFilter {
	return &nodeFilter{
		core: bloom.NewWithEstimates(cap, 0.001),
		cap:  cap,
	}
}

func (filter *nodeFilter) AddNode(nd *Node) bool {
	filter.lock.Lock()
	filter.lock.Unlock()

	key := fmt.Sprintf("%s:%d", nd.Addr.IP, nd.Addr.Port)
	filter.core.AddString(key)
	if !filter.core.TestAndAddString(key) {
		filter.amount++
		return false
	}
	return true
}

func (filter *nodeFilter) Check(nd *Node) bool {
	filter.lock.RLock()
	defer filter.lock.RUnlock()

	key := fmt.Sprintf("%s:%d", nd.Addr.IP, nd.Addr.Port)
	return !filter.core.TestString(key)
}

func (filter *nodeFilter) Reset() {
	filter.lock.Lock()
	defer filter.lock.Unlock()
	filter.core.ClearAll()
	filter.amount = 0
}

func (filter *nodeFilter) IsFull() bool {
	filter.lock.RLock()
	defer filter.lock.RUnlock()
	return filter.amount >= filter.cap
}
