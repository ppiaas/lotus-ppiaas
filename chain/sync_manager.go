package chain

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/types"

	peer "github.com/libp2p/go-libp2p-core/peer"
)

var BootstrapPeerThreshold = 2

var coalesceForksParents = false

func init() {
	if os.Getenv("LOTUS_SYNC_REL_PARENT") == "yes" {
		coalesceForksParents = true
	}
}

type SyncFunc func(context.Context, *types.TipSet) error

// SyncManager manages the chain synchronization process, both at bootstrap time
// and during ongoing operation.
//
// It receives candidate chain heads in the form of tipsets from peers,
// and schedules them onto sync workers, deduplicating processing for
// already-active syncs.
type SyncManager interface {
	// Start starts the SyncManager.
	Start()

	// Stop stops the SyncManager.
	Stop()

	// SetPeerHead informs the SyncManager that the supplied peer reported the
	// supplied tipset.
	SetPeerHead(ctx context.Context, p peer.ID, ts *types.TipSet)

	// State retrieves the state of the sync workers.
	State() []SyncerStateSnapshot
}

type syncManager struct {
	ctx    context.Context
	cancel func()

	workq   chan peerHead
	statusq chan workerStatus

	nextWorker uint64
	pend       syncBucketSet
	heads      map[peer.ID]*types.TipSet

	mx    sync.Mutex
	state map[uint64]*workerState

	doSync func(context.Context, *types.TipSet) error
}

var _ SyncManager = (*syncManager)(nil)

type peerHead struct {
	p  peer.ID
	ts *types.TipSet
}

type workerState struct {
	id uint64
	ts *types.TipSet
	ss *SyncerState
}

type workerStatus struct {
	id  uint64
	err error
}

// sync manager interface
func NewSyncManager(sync SyncFunc) SyncManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &syncManager{
		ctx:    ctx,
		cancel: cancel,

		workq:   make(chan peerHead),
		statusq: make(chan workerStatus),

		heads: make(map[peer.ID]*types.TipSet),
		state: make(map[uint64]*workerState),

		doSync: sync,
	}
}

func (sm *syncManager) Start() {
	go sm.scheduler()
}

func (sm *syncManager) Stop() {
	select {
	case <-sm.ctx.Done():
	default:
		sm.cancel()
	}
}

func (sm *syncManager) SetPeerHead(ctx context.Context, p peer.ID, ts *types.TipSet) {
	select {
	case sm.workq <- peerHead{p: p, ts: ts}:
	case <-sm.ctx.Done():
	case <-ctx.Done():
	}
}

func (sm *syncManager) State() []SyncerStateSnapshot {
	sm.mx.Lock()
	workerStates := make([]*workerState, 0, len(sm.state))
	for _, ws := range sm.state {
		workerStates = append(workerStates, ws)
	}
	sm.mx.Unlock()

	sort.Slice(workerStates, func(i, j int) bool {
		return workerStates[i].id < workerStates[j].id
	})

	result := make([]SyncerStateSnapshot, 0, len(workerStates))
	for _, ws := range workerStates {
		result = append(result, ws.ss.Snapshot())
	}

	return result
}

// sync manager internals
func (sm *syncManager) scheduler() {
	for {
		select {
		case head := <-sm.workq:
			sm.handlePeerHead(head)
		case status := <-sm.statusq:
			sm.handleWorkerStatus(status)
		case <-sm.ctx.Done():
			return
		}
	}
}

func (sm *syncManager) handlePeerHead(head peerHead) {
	log.Infof("new peer head: %s %s", head.p, head.ts)

	// have we started syncing yet?
	if sm.nextWorker == 0 {
		// track the peer head until we start syncing
		sm.heads[head.p] = head.ts

		// not yet; do we have enough peers?
		if len(sm.heads) < BootstrapPeerThreshold {
			// not enough peers; track it and wait
			return
		}

		// we are ready to start syncing; select the sync target and spawn a worker
		target, err := sm.selectInitialSyncTarget()
		if err != nil {
			log.Errorf("failed to select initial sync target: %s", err)
			return
		}

		log.Infof("selected initial sync target: %s", target)
		sm.spawnWorker(target)
		return
	}

	// we have started syncing, add peer head to the queue if applicable and maybe spawn a worker
	// if there is work to do (possibly in a fork)
	target, work, err := sm.addSyncTarget(head.ts)
	if err != nil {
		log.Warnf("failed to add sync target: %s", err)
		return
	}

	if work {
		log.Infof("selected sync target: %s", target)
		sm.spawnWorker(target)
	}
}

func (sm *syncManager) handleWorkerStatus(status workerStatus) {
	log.Debugf("worker %d done; status error: %s", status.err)

	sm.mx.Lock()
	ws := sm.state[status.id]
	delete(sm.state, status.id)
	sm.mx.Unlock()

	if status.err != nil {
		// we failed to sync this target -- log it and try to work on an extended chain
		// if there is nothing related to be worked on, we stop working on this chain.
		log.Errorf("error during sync in %s: %s", ws.ts, status.err)
	}

	// we are done with this target, select the next sync target and spawn a worker if there is work
	// to do, because of an extension of this chain.
	target, work, err := sm.selectSyncTarget(ws.ts)
	if err != nil {
		log.Warnf("failed to select sync target: %s", err)
		return
	}

	if work {
		log.Infof("selected sync target: %s", target)
		sm.spawnWorker(target)
	}
}

func (sm *syncManager) spawnWorker(target *types.TipSet) {
	id := sm.nextWorker
	sm.nextWorker++
	ws := &workerState{
		id: id,
		ts: target,
		ss: new(SyncerState),
	}

	sm.mx.Lock()
	sm.state[id] = ws
	sm.mx.Unlock()

	go sm.worker(ws)
}

func (sm *syncManager) worker(ws *workerState) {
	log.Infof("worker %d syncing in %s", ws.id, ws.ss)

	start := build.Clock.Now()
	defer func() {
		log.Infof("worker %d done; took %s", ws.id, build.Clock.Since(start))
	}()

	ctx := context.WithValue(sm.ctx, syncStateKey{}, ws.ss)
	err := sm.doSync(ctx, ws.ts)

	select {
	case sm.statusq <- workerStatus{id: ws.id, err: err}:
	case <-sm.ctx.Done():
	}
}

// selects the initial sync target by examining known peer heads; only called once for the initial
// sync.
func (sm *syncManager) selectInitialSyncTarget() (*types.TipSet, error) {
	var buckets syncBucketSet

	var peerHeads []*types.TipSet
	for _, ts := range sm.heads {
		peerHeads = append(peerHeads, ts)
	}
	// clear the map, we don't use it any longer
	sm.heads = nil

	sort.Slice(peerHeads, func(i, j int) bool {
		return peerHeads[i].Height() < peerHeads[j].Height()
	})

	for _, ts := range peerHeads {
		buckets.Insert(ts)
	}

	if len(buckets.buckets) > 1 {
		log.Warn("caution, multiple distinct chains seen during head selections")
		// TODO: we *could* refuse to sync here without user intervention.
		// For now, just select the best cluster
	}

	return buckets.Heaviest(), nil
}

// adds a tipset to the potential sync targets; returns true if there is a a tipset to work on.
// this could be either a restart, eg because there is no currently scheduled sync work or a worker
// failed or a potential fork.
func (sm *syncManager) addSyncTarget(ts *types.TipSet) (*types.TipSet, bool, error) {
	// Note: we don't need the state lock here to access the active worker states, as the only
	//       competing threads that may access it do so through State() which is read only.

	// if the worker set is empty, we have finished syncing and were waiting for the next tipset
	// in this case, we just return the tipset as work to be done
	if len(sm.state) == 0 {
		return ts, true, nil
	}

	// check if it is related to any active sync; if so insert into the pending sync queue
	for _, ws := range sm.state {
		if ts.Equals(ws.ts) {
			// ignore it, we are already syncing it
			return nil, false, nil
		}

		if ts.Parents() == ws.ts.Key() {
			// schedule for syncing next; it's an extension of an active sync
			sm.pend.Insert(ts)
			return nil, false, nil
		}
	}

	// check to see if it is related to any pending sync; if so insert it into the pending sync queue
	if sm.pend.RelatedToAny(ts) {
		sm.pend.Insert(ts)
		return nil, false, nil
	}

	// it's not related to any active or pending sync; this could be a fork in which case we
	// start a new worker to sync it, if it is *heavier* than any active or pending set;
	// if it is not, we ignore it.
	activeHeavier := false
	for _, ws := range sm.state {
		if ws.ts.Height() > ts.Height() {
			activeHeavier = true
			break
		}
	}

	if activeHeavier {
		return nil, false, nil
	}

	pendHeaviest := sm.pend.Heaviest()
	if pendHeaviest != nil && pendHeaviest.Height() > ts.Height() {
		return nil, false, nil
	}

	// start a new worker, seems heavy enough and unrelated to active or pending syncs
	return ts, true, nil
}

// selects the next sync target after a worker sync has finished; returns true and a target
// TipSet if this chain should continue to sync because there is a heavier related tipset.
func (sm *syncManager) selectSyncTarget(done *types.TipSet) (*types.TipSet, bool, error) {
	// we pop the related bucket and if there is any related tipset, we work on the heaviest one next
	// if we are not already working on a heavier tipset
	related := sm.pend.PopRelated(done)
	if related == nil {
		return nil, false, nil
	}

	heaviest := related.heaviestTipSet()
	for _, ws := range sm.state {
		if ws.ts.Height() > heaviest.Height() {
			return nil, false, nil
		}
	}

	return heaviest, true, nil
}

// sync buckets and related utilities
type syncBucketSet struct {
	buckets []*syncTargetBucket
}

type syncTargetBucket struct {
	tips []*types.TipSet
}

func newSyncTargetBucket(tipsets ...*types.TipSet) *syncTargetBucket {
	var stb syncTargetBucket
	for _, ts := range tipsets {
		stb.add(ts)
	}
	return &stb
}

func (sbs *syncBucketSet) String() string {
	var bStrings []string
	for _, b := range sbs.buckets {
		var tsStrings []string
		for _, t := range b.tips {
			tsStrings = append(tsStrings, t.String())
		}
		bStrings = append(bStrings, "["+strings.Join(tsStrings, ",")+"]")
	}

	return "{" + strings.Join(bStrings, ";") + "}"
}

func (sbs *syncBucketSet) RelatedToAny(ts *types.TipSet) bool {
	for _, b := range sbs.buckets {
		if b.sameChainAs(ts) {
			return true
		}
	}
	return false
}

func (sbs *syncBucketSet) Insert(ts *types.TipSet) {
	for _, b := range sbs.buckets {
		if b.sameChainAs(ts) {
			b.add(ts)
			return
		}
	}
	sbs.buckets = append(sbs.buckets, newSyncTargetBucket(ts))
}

func (sbs *syncBucketSet) Pop() *syncTargetBucket {
	var bestBuck *syncTargetBucket
	var bestTs *types.TipSet
	for _, b := range sbs.buckets {
		hts := b.heaviestTipSet()
		if bestBuck == nil || bestTs.ParentWeight().LessThan(hts.ParentWeight()) {
			bestBuck = b
			bestTs = hts
		}
	}

	sbs.removeBucket(bestBuck)

	return bestBuck
}

func (sbs *syncBucketSet) removeBucket(toremove *syncTargetBucket) {
	nbuckets := make([]*syncTargetBucket, 0, len(sbs.buckets)-1)
	for _, b := range sbs.buckets {
		if b != toremove {
			nbuckets = append(nbuckets, b)
		}
	}
	sbs.buckets = nbuckets
}

func (sbs *syncBucketSet) PopRelated(ts *types.TipSet) *syncTargetBucket {
	var bOut *syncTargetBucket
	for _, b := range sbs.buckets {
		if b.sameChainAs(ts) {
			sbs.removeBucket(b)
			if bOut == nil {
				bOut = &syncTargetBucket{}
			}
			bOut.tips = append(bOut.tips, b.tips...)
		}
	}
	return bOut
}

func (sbs *syncBucketSet) Heaviest() *types.TipSet {
	// TODO: should also consider factoring in number of peers represented by each bucket here
	var bestTs *types.TipSet
	for _, b := range sbs.buckets {
		bhts := b.heaviestTipSet()
		if bestTs == nil || bhts.ParentWeight().GreaterThan(bestTs.ParentWeight()) {
			bestTs = bhts
		}
	}
	return bestTs
}

func (sbs *syncBucketSet) Empty() bool {
	return len(sbs.buckets) == 0
}

func (stb *syncTargetBucket) sameChainAs(ts *types.TipSet) bool {
	for _, t := range stb.tips {
		if ts.Equals(t) {
			return true
		}
		if ts.Key() == t.Parents() {
			return true
		}
		if ts.Parents() == t.Key() {
			return true
		}
		if coalesceForksParents && ts.Parents() == t.Parents() {
			return true
		}
	}
	return false
}

func (stb *syncTargetBucket) add(ts *types.TipSet) {

	for _, t := range stb.tips {
		if t.Equals(ts) {
			return
		}
	}

	stb.tips = append(stb.tips, ts)
}

func (stb *syncTargetBucket) heaviestTipSet() *types.TipSet {
	if stb == nil {
		return nil
	}

	var best *types.TipSet
	for _, ts := range stb.tips {
		if best == nil || ts.ParentWeight().GreaterThan(best.ParentWeight()) {
			best = ts
		}
	}
	return best
}
