// Package blockservice implements a BlockService interface that provides
// a single GetBlock/AddBlock interface that seamlessly retrieves data either
// locally or from a remote peer through the exchange.
package blockservice

import (
	"context"
	"io"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/exchange"
	"github.com/ipfs/boxo/provider"
	"github.com/ipfs/boxo/verifcid"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"

	"github.com/ipfs/boxo/blockservice/internal"
)

var logger = logging.Logger("blockservice")

// BlockGetter is the common interface shared between blockservice sessions and
// the blockservice.
type BlockGetter interface {
	// GetBlock gets the requested block.
	GetBlock(ctx context.Context, c cid.Cid) (blocks.Block, error)

	// GetBlocks does a batch request for the given cids, returning blocks as
	// they are found, in no particular order.
	//
	// It may not be able to find all requested blocks (or the context may
	// be canceled). In that case, it will close the channel early. It is up
	// to the consumer to detect this situation and keep track which blocks
	// it has received and which it hasn't.
	GetBlocks(ctx context.Context, ks []cid.Cid) <-chan blocks.Block
}

// BlockService is a hybrid block datastore. It stores data in a local
// datastore and may retrieve data from a remote Exchange.
// It uses an internal `datastore.Datastore` instance to store values.
type BlockService interface {
	io.Closer
	BlockGetter

	// Blockstore returns a reference to the underlying blockstore
	Blockstore() blockstore.Blockstore

	// Exchange returns a reference to the underlying exchange (usually bitswap)
	Exchange() exchange.Interface

	// AddBlock puts a given block to the underlying datastore
	AddBlock(ctx context.Context, o blocks.Block) error

	// AddBlocks adds a slice of blocks at the same time using batching
	// capabilities of the underlying datastore whenever possible.
	AddBlocks(ctx context.Context, bs []blocks.Block) error

	// DeleteBlock deletes the given block from the blockservice.
	DeleteBlock(ctx context.Context, o cid.Cid) error
}

// BoundedBlockService is a Blockservice bounded via strict multihash Allowlist.
type BoundedBlockService interface {
	BlockService

	Allowlist() verifcid.Allowlist
}

var _ BoundedBlockService = (*blockService)(nil)

// ProvidingBlockService is a Blockservice which provides new blocks to a provider.
type ProvidingBlockService interface {
	BlockService

	// Provider can return nil, then no provider is used.
	Provider() provider.Provider
}

var _ ProvidingBlockService = (*blockService)(nil)

type blockService struct {
	allowlist  verifcid.Allowlist
	blockstore blockstore.Blockstore
	exchange   exchange.Interface
	provider   provider.Provider
	// If checkFirst is true then first check that a block doesn't
	// already exist to avoid republishing the block on the exchange.
	checkFirst bool
}

type Option func(*blockService)

// WriteThrough disable cache checks for writes and make them go straight to
// the blockstore.
func WriteThrough() Option {
	return func(bs *blockService) {
		bs.checkFirst = false
	}
}

// WithAllowlist sets a custom [verifcid.Allowlist] which will be used
func WithAllowlist(allowlist verifcid.Allowlist) Option {
	return func(bs *blockService) {
		bs.allowlist = allowlist
	}
}

// WithProvider allows to advertise anything that is added through the blockservice.
func WithProvider(prov provider.Provider) Option {
	return func(bs *blockService) {
		bs.provider = prov
	}
}

// New creates a BlockService with given datastore instance.
func New(bs blockstore.Blockstore, exchange exchange.Interface, opts ...Option) BlockService {
	if exchange == nil {
		logger.Debug("blockservice running in local (offline) mode.")
	}

	service := &blockService{
		allowlist:  verifcid.DefaultAllowlist,
		blockstore: bs,
		exchange:   exchange,
		checkFirst: true,
	}

	for _, opt := range opts {
		opt(service)
	}

	return service
}

// Blockstore returns the blockstore behind this blockservice.
func (s *blockService) Blockstore() blockstore.Blockstore {
	return s.blockstore
}

// Exchange returns the exchange behind this blockservice.
func (s *blockService) Exchange() exchange.Interface {
	return s.exchange
}

func (s *blockService) Allowlist() verifcid.Allowlist {
	return s.allowlist
}

func (s *blockService) Provider() provider.Provider {
	return s.provider
}

// NewSession creates a new session that allows for
// controlled exchange of wantlists to decrease the bandwidth overhead.
// If the current exchange is a SessionExchange, a new exchange
// session will be created. Otherwise, the current exchange will be used
// directly.
// Sessions are lazily setup, this is cheap.
func NewSession(ctx context.Context, bs BlockService) *Session {
	return &Session{bs: bs, sesctx: ctx}
}

// AddBlock adds a particular block to the service, Putting it into the datastore.
func (s *blockService) AddBlock(ctx context.Context, o blocks.Block) error {
	ctx, span := internal.StartSpan(ctx, "blockService.AddBlock")
	defer span.End()

	c := o.Cid()
	err := verifcid.ValidateCid(s.allowlist, c) // hash security
	if err != nil {
		return err
	}
	if s.checkFirst {
		if has, err := s.blockstore.Has(ctx, c); has || err != nil {
			return err
		}
	}

	if err := s.blockstore.Put(ctx, o); err != nil {
		return err
	}

	logger.Debugf("BlockService.BlockAdded %s", c)

	if s.exchange != nil {
		if err := s.exchange.NotifyNewBlocks(ctx, o); err != nil {
			logger.Errorf("NotifyNewBlocks: %s", err.Error())
		}
	}
	if s.provider != nil {
		if err := s.provider.Provide(o.Cid()); err != nil {
			logger.Errorf("Provide: %s", err.Error())
		}
	}

	return nil
}

func (s *blockService) AddBlocks(ctx context.Context, bs []blocks.Block) error {
	ctx, span := internal.StartSpan(ctx, "blockService.AddBlocks")
	defer span.End()

	// hash security
	for _, b := range bs {
		err := verifcid.ValidateCid(s.allowlist, b.Cid())
		if err != nil {
			return err
		}
	}
	var toput []blocks.Block
	if s.checkFirst {
		toput = make([]blocks.Block, 0, len(bs))
		for _, b := range bs {
			has, err := s.blockstore.Has(ctx, b.Cid())
			if err != nil {
				return err
			}
			if !has {
				toput = append(toput, b)
			}
		}
	} else {
		toput = bs
	}

	if len(toput) == 0 {
		return nil
	}

	err := s.blockstore.PutMany(ctx, toput)
	if err != nil {
		return err
	}

	if s.exchange != nil {
		logger.Debugf("BlockService.BlockAdded %d blocks", len(toput))
		if err := s.exchange.NotifyNewBlocks(ctx, toput...); err != nil {
			logger.Errorf("NotifyNewBlocks: %s", err.Error())
		}
	}
	if s.provider != nil {
		for _, o := range toput {
			if err := s.provider.Provide(o.Cid()); err != nil {
				logger.Errorf("Provide: %s", err.Error())
			}
		}
	}

	return nil
}

// GetBlock retrieves a particular block from the service,
// Getting it from the datastore using the key (hash).
func (s *blockService) GetBlock(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	ctx, span := internal.StartSpan(ctx, "blockService.GetBlock", trace.WithAttributes(attribute.Stringer("CID", c)))
	defer span.End()

	return getBlock(ctx, c, s, s.getExchangeFetcher)
}

// Look at what I have to do, no interface covariance :'(
func (s *blockService) getExchangeFetcher() exchange.Fetcher {
	return s.exchange
}

func getBlock(ctx context.Context, c cid.Cid, bs BlockService, fetchFactory func() exchange.Fetcher) (blocks.Block, error) {
	err := verifcid.ValidateCid(grabAllowlistFromBlockservice(bs), c) // hash security
	if err != nil {
		return nil, err
	}

	blockstore := bs.Blockstore()

	block, err := blockstore.Get(ctx, c)
	switch {
	case err == nil:
		return block, nil
	case ipld.IsNotFound(err):
		break
	default:
		return nil, err
	}

	fetch := fetchFactory() // lazily create session if needed
	if fetch == nil {
		logger.Debug("BlockService GetBlock: Not found")
		return nil, err
	}

	logger.Debug("BlockService: Searching")
	blk, err := fetch.GetBlock(ctx, c)
	if err != nil {
		return nil, err
	}
	// also write in the blockstore for caching, inform the exchange that the block is available
	err = blockstore.Put(ctx, blk)
	if err != nil {
		return nil, err
	}
	if ex := bs.Exchange(); ex != nil {
		err = ex.NotifyNewBlocks(ctx, blk)
		if err != nil {
			return nil, err
		}
	}
	if provider := grabProviderFromBlockservice(bs); provider != nil {
		err = provider.Provide(blk.Cid())
		if err != nil {
			return nil, err
		}
	}
	logger.Debugf("BlockService.BlockFetched %s", c)
	return blk, nil
}

// GetBlocks gets a list of blocks asynchronously and returns through
// the returned channel.
// NB: No guarantees are made about order.
func (s *blockService) GetBlocks(ctx context.Context, ks []cid.Cid) <-chan blocks.Block {
	ctx, span := internal.StartSpan(ctx, "blockService.GetBlocks")
	defer span.End()

	return getBlocks(ctx, ks, s, s.getExchangeFetcher)
}

func getBlocks(ctx context.Context, ks []cid.Cid, blockservice BlockService, fetchFactory func() exchange.Fetcher) <-chan blocks.Block {
	out := make(chan blocks.Block)

	go func() {
		defer close(out)

		allowlist := grabAllowlistFromBlockservice(blockservice)

		var lastAllValidIndex int
		var c cid.Cid
		for lastAllValidIndex, c = range ks {
			if err := verifcid.ValidateCid(allowlist, c); err != nil {
				break
			}
		}

		if lastAllValidIndex != len(ks) {
			// can't shift in place because we don't want to clobber callers.
			ks2 := make([]cid.Cid, lastAllValidIndex, len(ks))
			copy(ks2, ks[:lastAllValidIndex])          // fast path for already filtered elements
			for _, c := range ks[lastAllValidIndex:] { // don't rescan already scanned elements
				// hash security
				if err := verifcid.ValidateCid(allowlist, c); err == nil {
					ks2 = append(ks2, c)
				} else {
					logger.Errorf("unsafe CID (%s) passed to blockService.GetBlocks: %s", c, err)
				}
			}
			ks = ks2
		}

		bs := blockservice.Blockstore()

		var misses []cid.Cid
		for _, c := range ks {
			hit, err := bs.Get(ctx, c)
			if err != nil {
				misses = append(misses, c)
				continue
			}
			select {
			case out <- hit:
			case <-ctx.Done():
				return
			}
		}

		fetch := fetchFactory() // don't load exchange unless we have to
		if len(misses) == 0 || fetch == nil {
			return
		}

		rblocks, err := fetch.GetBlocks(ctx, misses)
		if err != nil {
			logger.Debugf("Error with GetBlocks: %s", err)
			return
		}

		ex := blockservice.Exchange()
		provider := grabProviderFromBlockservice(blockservice)
		var cache [1]blocks.Block // preallocate once for all iterations
		for {
			var b blocks.Block
			select {
			case v, ok := <-rblocks:
				if !ok {
					return
				}
				b = v
			case <-ctx.Done():
				return
			}

			// write in the blockstore for caching
			err = bs.Put(ctx, b)
			if err != nil {
				logger.Errorf("could not write blocks from the network to the blockstore: %s", err)
				return
			}

			if ex != nil {
				// inform the exchange that the blocks are available
				cache[0] = b
				err = ex.NotifyNewBlocks(ctx, cache[:]...)
				if err != nil {
					logger.Errorf("could not tell the exchange about new blocks: %s", err)
					return
				}
				cache[0] = nil // early gc
			}

			if provider != nil {
				err = provider.Provide(b.Cid())
				if err != nil {
					logger.Errorf("could not tell the provider about new blocks: %s", err)
					return
				}
			}

			select {
			case out <- b:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// DeleteBlock deletes a block in the blockservice from the datastore
func (s *blockService) DeleteBlock(ctx context.Context, c cid.Cid) error {
	ctx, span := internal.StartSpan(ctx, "blockService.DeleteBlock", trace.WithAttributes(attribute.Stringer("CID", c)))
	defer span.End()

	err := s.blockstore.DeleteBlock(ctx, c)
	if err == nil {
		logger.Debugf("BlockService.BlockDeleted %s", c)
	}
	return err
}

func (s *blockService) Close() error {
	logger.Debug("blockservice is shutting down...")
	if s.exchange == nil {
		return nil
	}
	return s.exchange.Close()
}

// Session is a helper type to provide higher level access to bitswap sessions
type Session struct {
	createSession sync.Once
	bs            BlockService
	ses           exchange.Fetcher
	sesctx        context.Context
}

// grabSession is used to lazily create sessions.
func (s *Session) grabSession() exchange.Fetcher {
	s.createSession.Do(func() {
		defer func() {
			s.sesctx = nil // early gc
		}()

		ex := s.bs.Exchange()
		if ex == nil {
			return
		}
		s.ses = ex // always fallback to non session fetches

		sesEx, ok := ex.(exchange.SessionExchange)
		if !ok {
			return
		}
		s.ses = sesEx.NewSession(s.sesctx)
	})

	return s.ses
}

// GetBlock gets a block in the context of a request session
func (s *Session) GetBlock(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	ctx, span := internal.StartSpan(ctx, "Session.GetBlock", trace.WithAttributes(attribute.Stringer("CID", c)))
	defer span.End()

	return getBlock(ctx, c, s.bs, s.grabSession)
}

// GetBlocks gets blocks in the context of a request session
func (s *Session) GetBlocks(ctx context.Context, ks []cid.Cid) <-chan blocks.Block {
	ctx, span := internal.StartSpan(ctx, "Session.GetBlocks")
	defer span.End()

	return getBlocks(ctx, ks, s.bs, s.grabSession)
}

var _ BlockGetter = (*Session)(nil)

// grabAllowlistFromBlockservice never returns nil
func grabAllowlistFromBlockservice(bs BlockService) verifcid.Allowlist {
	if bbs, ok := bs.(BoundedBlockService); ok {
		return bbs.Allowlist()
	}
	return verifcid.DefaultAllowlist
}

// grabProviderFromBlockservice can return nil if no provider is used.
func grabProviderFromBlockservice(bs BlockService) provider.Provider {
	if bbs, ok := bs.(ProvidingBlockService); ok {
		return bbs.Provider()
	}
	return nil
}
