package node

import (
	"encoding/hex"
	lru "github.com/hashicorp/golang-lru"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	log "github.com/sirupsen/logrus"
	"go-chronos/common"
	"go-chronos/core"
	"go-chronos/p2p"
	"sync"
	"time"
)

type msgHandler func(h *Handler, msg *p2p.Message, p *Peer)

const (
	maxKnownBlock       = 1024
	maxKnownTransaction = 32768
	ProtocolId          = protocol.ID("/chronos/1.0.0")
)

var handlerMap = map[p2p.StatusCode]msgHandler{
	p2p.StatusCodeStatusMsg:                     handleStatusMsg,
	p2p.StatusCodeNewBlockMsg:                   handleNewBlockMsg,
	p2p.StatusCodeNewBlockHashesMsg:             handleNewBlockHashMsg,
	p2p.StatusCodeBlockBodiesMsg:                handleBlockMsg,
	p2p.StatusCodeTransactionsMsg:               handleTransactionMsg,
	p2p.StatusCodeNewPooledTransactionHashesMsg: handleNewPooledTransactionHashesMsg,
	p2p.StatusCodeGetPooledTransactionMsg:       handleGetPooledTransactionMsg,
	p2p.StatusCodeSyncStatusReq:                 handleSyncStatusReq,
	p2p.StatusCodeSyncStatusMsg:                 handleSyncStatusMsg,
	p2p.StatusCodeSyncGetBlocksMsg:              handleSyncGetBlocksMsg,
	p2p.StatusCodeSyncBlocksMsg:                 handleSyncBlockMsg,
}

var (
	handlerInst *Handler = nil
)

type HandlerConfig struct {
	TxPool *core.TxPool
	Chain  *core.BlockChain
}

type Handler struct {
	peerSet []*Peer

	blockBroadcastQueue chan *common.Block
	txBroadcastQueue    chan *common.Transaction

	knownBlock       *lru.Cache
	knownTransaction *lru.Cache

	txPool *core.TxPool
	chain  *core.BlockChain

	blockSyncer  *BlockSyncer
	startRoutine sync.Once

	peerSetLock sync.RWMutex
}

// HandleStream 用于在收到对端连接时候处理 stream, 在这里构建 peer 用于通信
func HandleStream(s network.Stream) {
	//ctx := GetPeerContext()
	handler := GetHandlerInst()
	conn := s.Conn()
	//peer, err := NewPeer(ctx, conn.RemotePeer(), &conn)
	_, err := handler.NewPeer(conn.RemotePeer(), &s)

	log.Infoln("Receive new stream, handle stream.")

	if err != nil {
		log.WithField("error", err).Errorln("Handle stream error.")
		return
	}
}

func NewHandler(config *HandlerConfig) (*Handler, error) {
	knownBlockCache, err := lru.New(maxKnownBlock)

	if err != nil {
		log.WithField("error", err).Debugln("Create known block cache failed.")
		return nil, err
	}

	knownTxCache, err := lru.New(maxKnownTransaction)

	if err != nil {
		log.WithField("error", err).Debugln("Create known transaction cache failed.")
		return nil, err
	}

	blockSyncerConfig := &BlockSyncerConfig{
		Chain: config.Chain,
	}
	syncer := NewBlockSyncer(blockSyncerConfig)

	handler := &Handler{
		// todo: 限制节点数量，Kad 应该限制了节点数量不超过20个
		peerSet: make([]*Peer, 0, 40),

		blockBroadcastQueue: make(chan *common.Block, 64),
		txBroadcastQueue:    make(chan *common.Transaction, 8192),

		knownBlock:       knownBlockCache,
		knownTransaction: knownTxCache,

		txPool: config.TxPool,
		chain:  config.Chain,

		blockSyncer: syncer,
	}

	go handler.packageBlockRoutine()
	syncer.Start()
	handlerInst = handler

	return handler, nil
}

// AddTransaction 仅用于测试
func (h *Handler) AddTransaction(tx *common.Transaction) {
	h.txBroadcastQueue <- tx
	h.txPool.Add(tx)
}

func GetHandlerInst() *Handler {
	// todo: 这样的写法会有脏读的问题，也就是在分配地址后，对象可能还没有完全初始化
	return handlerInst
}

func (h *Handler) packageBlockRoutine() {
	ticker := time.NewTicker(1 * time.Second)

	for {
		select {
		case <-ticker.C:
			if !h.synced() {
				continue
			}

			//log.Debugln("Start package block.")
			latest, _ := h.chain.GetLatestBlock()

			if latest == nil {
				log.Debugln("Waiting for genesis block.")
				continue
			}

			txs := h.txPool.Package()
			log.Debugf("Package %d txs.", len(txs))
			newBlock, err := h.chain.PackageNewBlock(txs)

			if err != nil {
				log.WithField("error", err).Debugln("Package new block failed.")
				continue
			}

			h.chain.AppendBlockTask(newBlock)
			h.blockBroadcastQueue <- newBlock
			log.Debugf("Package new block# 0x%s", hex.EncodeToString(newBlock.Header.BlockHash[:]))
		}
	}
}

func (h *Handler) NewPeer(peerId peer.ID, s *network.Stream) (*Peer, error) {
	config := PeerConfig{
		chain:   h.chain,
		txPool:  h.txPool,
		handler: h,
	}

	p, err := NewPeer(peerId, s, config)

	if err != nil {
		log.WithField("error", err).Errorln("Create peer failed.")
		return nil, err
	}

	h.peerSetLock.Lock()
	h.peerSetLock.Unlock()

	h.peerSet = append(h.peerSet, p)
	h.blockSyncer.AddPeer(p)
	return p, err
}

func (h *Handler) isKnownTransaction(hash common.Hash) bool {
	strHash := hex.EncodeToString(hash[:])
	if h.txPool.Contain(strHash) {
		return true
	}

	tx, err := h.chain.GetTransactionByHash(hash)

	if err != nil {
		return false
	}

	return tx != nil
}

func (h *Handler) broadcastBlock() {
	for {
		select {
		case block := <-h.blockBroadcastQueue:
			blockHash := block.Header.BlockHash

			peers := h.getPeersWithoutBlock(blockHash)

			peersCount := len(peers)
			//countBroadcastBlock := int(math.Sqrt(float64(peersCount)))
			//todo: 这里是由于获取新区块的逻辑还没有，所以先全部广播
			//  在后续完成对应的逻辑后，再修改这里的逻辑来降低广播的时间复杂度
			countBroadcastBlock := peersCount
			log.WithField("count", countBroadcastBlock).Debugln("Broadcast block.")

			for idx := 0; idx < countBroadcastBlock; idx++ {
				peers[idx].AsyncSendNewBlock(block)
			}

			for idx := countBroadcastBlock; idx < peersCount; idx++ {
				peers[idx].AsyncSendNewBlockHash(block.Header.BlockHash)
			}
		}
	}
}

func (h *Handler) broadcastTransaction() {
	for {
		select {
		case tx := <-h.txBroadcastQueue:
			txHash := tx.Body.Hash
			peers := h.getPeersWithoutTransaction(txHash)

			peersCount := len(peers)

			//todo: 这里是由于获取新区块的逻辑还没有，所以先全部广播
			//  在后续完成对应的逻辑后，再修改这里的逻辑来降低广播的时间复杂度
			//countBroadcastBody := int(math.Sqrt(float64(peersCount)))
			countBroadcastBody := peersCount

			for idx := 0; idx < countBroadcastBody; idx++ {
				peers[idx].AsyncSendTransaction(tx)
			}

			for idx := countBroadcastBody; idx < peersCount; idx++ {
				peers[idx].AsyncSendTxHash(txHash)
			}
		}
	}
}

func (h *Handler) getPeersWithoutBlock(blockHash common.Hash) []*Peer {
	h.peerSetLock.RLock()
	defer h.peerSetLock.RUnlock()

	list := make([]*Peer, 0, len(h.peerSet))
	strHash := hex.EncodeToString(blockHash[:])
	for idx := range h.peerSet {
		p := h.peerSet[idx]
		if !p.KnownBlock(strHash) && !p.Stopped() {
			list = append(list, p)
		}
	}
	return list
}

func (h *Handler) appendBlockToSyncer(block *common.Block) {
	h.blockSyncer.appendBlock(block)
}

func (h *Handler) getPeersWithoutTransaction(txHash common.Hash) []*Peer {
	h.peerSetLock.RLock()
	defer h.peerSetLock.RUnlock()

	list := make([]*Peer, 0, len(h.peerSet))
	strHash := hex.EncodeToString(txHash[:])
	for idx := range h.peerSet {
		p := h.peerSet[idx]
		if !p.KnownTransaction(strHash) && !p.Stopped() {
			list = append(list, p)
		}
	}
	return list
}

func (h *Handler) SetSynced() {
	h.blockSyncer.setSynced()
}

func (h *Handler) markBlock(hash string) {
	h.knownBlock.Add(hash, nil)
}

func (h *Handler) markTransaction(hash string) {
	h.knownTransaction.Add(hash, nil)
}

func (h *Handler) syncStatus() uint8 {
	return h.blockSyncer.status
}

func (h *Handler) synced() bool {
	status := h.blockSyncer.getStatus()
	if status == synced {
		h.startRoutine.Do(func() {
			log.Traceln("Block sync finish!")
			go h.broadcastBlock()
			go h.broadcastTransaction()
		})
		return true
	}
	return false
}

// StatusMessage 生成同步信息给对端
func (h *Handler) StatusMessage() *p2p.SyncStatusMsg {
	block, err := h.chain.GetLatestBlock()

	if err != nil {
		return &p2p.SyncStatusMsg{
			LatestHeight:        -1,
			LatestHash:          [32]byte{},
			BufferedStartHeight: 0,
			BufferedEndHeight:   -1,
		}
	}

	return &p2p.SyncStatusMsg{
		LatestHeight:        block.Header.Height,
		LatestHash:          block.Header.BlockHash,
		BufferedStartHeight: 0,
		BufferedEndHeight:   h.chain.BufferedHeight(),
	}
}

// removePeerIfStopped 移除断开连接的 Peer
// todo: 这里的方法感觉有点复杂，以后需要优化
// todo: 这里不能在遍历的时候进行移除
func (h *Handler) removePeerIfStopped(idx int) {
	h.peerSetLock.Lock()
	defer h.peerSetLock.Unlock()

	p := h.peerSet[idx]
	if p.Stopped() {
		h.peerSet = append(h.peerSet[:idx], h.peerSet[idx+1:]...)
	}
}
