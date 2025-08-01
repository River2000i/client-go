// Copyright 2021 TiKV Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// NOTE: The code in this file is based on code from the
// TiDB project, licensed under the Apache License v 2.0
//
// https://github.com/pingcap/tidb/tree/cc5e161ac06827589c4966674597c137cc9e809c/store/tikv/client/client_batch.go
//

// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package client provides tcp connection to kvserver.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"runtime/trace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/kvproto/pkg/tikvpb"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tikv/client-go/v2/config"
	"github.com/tikv/client-go/v2/config/retry"
	tikverr "github.com/tikv/client-go/v2/error"
	"github.com/tikv/client-go/v2/internal/logutil"
	"github.com/tikv/client-go/v2/metrics"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/util"
	"github.com/tikv/client-go/v2/util/async"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/metadata"
)

type batchCommandsEntry struct {
	ctx context.Context
	req *tikvpb.BatchCommandsRequest_Request
	res chan *tikvpb.BatchCommandsResponse_Response
	cb  async.Callback[*tikvrpc.Response]
	// forwardedHost is the address of a store which will handle the request.
	// It's different from the address the request sent to.
	forwardedHost string
	// canceled indicated the request is canceled or not.
	canceled int32
	err      error
	pri      uint64

	// start indicates when the batch commands entry is generated and sent to the batch conn channel.
	start   time.Time
	sendLat int64
	recvLat int64
}

func (b *batchCommandsEntry) isCanceled() bool {
	return atomic.LoadInt32(&b.canceled) == 1
}

func (b *batchCommandsEntry) priority() uint64 {
	return b.pri
}

func (b *batchCommandsEntry) async() bool {
	return b.cb != nil
}

func (b *batchCommandsEntry) response(resp *tikvpb.BatchCommandsResponse_Response) {
	if b.async() {
		b.cb.Schedule(tikvrpc.FromBatchCommandsResponse(resp))
	} else {
		b.res <- resp
	}
}

func (b *batchCommandsEntry) error(err error) {
	b.err = err
	if b.async() {
		b.cb.Schedule(nil, err)
	} else {
		close(b.res)
	}
}

// batchCommandsBuilder collects a batch of `batchCommandsEntry`s to build
// `BatchCommandsRequest`s.
type batchCommandsBuilder struct {
	// Each BatchCommandsRequest_Request sent to a store has a unique identity to
	// distinguish its response.
	idAlloc    uint64
	entries    *PriorityQueue
	requests   []*tikvpb.BatchCommandsRequest_Request
	requestIDs []uint64
	// In most cases, there isn't any forwardingReq.
	forwardingReqs map[string]*tikvpb.BatchCommandsRequest

	latestReqStartTime time.Time
}

func (b *batchCommandsBuilder) len() int {
	return b.entries.Len()
}

func (b *batchCommandsBuilder) push(entry *batchCommandsEntry) {
	b.entries.Push(entry)
	if entry.start.After(b.latestReqStartTime) {
		b.latestReqStartTime = entry.start
	}
}

const highTaskPriority = 10

func (b *batchCommandsBuilder) hasHighPriorityTask() bool {
	return b.entries.highestPriority() >= highTaskPriority
}

// buildWithLimit builds BatchCommandsRequests with the given limit.
// the highest priority tasks don't consume any limit,
// so the limit only works for normal tasks.
// The first return value is the request that doesn't need forwarding.
// The second is a map that maps forwarded hosts to requests.
func (b *batchCommandsBuilder) buildWithLimit(limit int64, collect func(id uint64, e *batchCommandsEntry),
) (*tikvpb.BatchCommandsRequest, map[string]*tikvpb.BatchCommandsRequest) {
	count := int64(0)
	build := func(reqs []Item) {
		for _, e := range reqs {
			e := e.(*batchCommandsEntry)
			if e.isCanceled() {
				continue
			}
			if e.priority() < highTaskPriority {
				count++
			}

			if collect != nil {
				collect(b.idAlloc, e)
			}
			if e.forwardedHost == "" {
				b.requestIDs = append(b.requestIDs, b.idAlloc)
				b.requests = append(b.requests, e.req)
			} else {
				batchReq, ok := b.forwardingReqs[e.forwardedHost]
				if !ok {
					batchReq = &tikvpb.BatchCommandsRequest{}
					b.forwardingReqs[e.forwardedHost] = batchReq
				}
				batchReq.RequestIds = append(batchReq.RequestIds, b.idAlloc)
				batchReq.Requests = append(batchReq.Requests, e.req)
			}
			b.idAlloc++
		}
	}
	for (count < limit && b.entries.Len() > 0) || b.hasHighPriorityTask() {
		n := limit
		if limit == 0 {
			n = 1
		}
		reqs := b.entries.Take(int(n))
		if len(reqs) == 0 {
			break
		}
		build(reqs)
	}
	var req *tikvpb.BatchCommandsRequest
	if len(b.requests) > 0 {
		req = &tikvpb.BatchCommandsRequest{
			Requests:   b.requests,
			RequestIds: b.requestIDs,
		}
	}
	return req, b.forwardingReqs
}

// cancel all requests, only used in test.
func (b *batchCommandsBuilder) cancel(e error) {
	for _, entry := range b.entries.all() {
		entry.(*batchCommandsEntry).error(e)
	}
	b.entries.reset()
}

// reset resets the builder to the initial state.
// Should call it before collecting a new batch.
func (b *batchCommandsBuilder) reset() {
	b.entries.clean()
	// NOTE: We can't simply set entries = entries[:0] here.
	// The data in the cap part of the slice would reference the prewrite keys whose
	// underlying memory is borrowed from memdb. The reference cause GC can't release
	// the memdb, leading to serious memory leak problems in the large transaction case.
	for i := 0; i < len(b.requests); i++ {
		b.requests[i] = nil
	}
	b.requests = b.requests[:0]
	b.requestIDs = b.requestIDs[:0]

	for k := range b.forwardingReqs {
		delete(b.forwardingReqs, k)
	}
}

func newBatchCommandsBuilder(maxBatchSize uint) *batchCommandsBuilder {
	return &batchCommandsBuilder{
		idAlloc:        0,
		entries:        NewPriorityQueue(),
		requests:       make([]*tikvpb.BatchCommandsRequest_Request, 0, maxBatchSize),
		requestIDs:     make([]uint64, 0, maxBatchSize),
		forwardingReqs: make(map[string]*tikvpb.BatchCommandsRequest),
	}
}

const (
	batchSendTailLatThreshold = 20 * time.Millisecond
	batchRecvTailLatThreshold = 20 * time.Millisecond
)

type batchConnMetrics struct {
	pendingRequests prometheus.Observer
	batchSize       prometheus.Observer

	sendLoopWaitHeadDur prometheus.Observer
	sendLoopWaitMoreDur prometheus.Observer
	sendLoopSendDur     prometheus.Observer

	recvLoopRecvDur    prometheus.Observer
	recvLoopProcessDur prometheus.Observer

	batchSendTailLat prometheus.Observer
	batchRecvTailLat prometheus.Observer

	headArrivalInterval prometheus.Observer
	batchMoreRequests   prometheus.Observer

	bestBatchSize prometheus.Observer
}

type batchConn struct {
	// An atomic flag indicates whether the batch is idle or not.
	// 0 for busy, others for idle.
	idle uint32

	// batchCommandsCh used for batch commands.
	batchCommandsCh        chan *batchCommandsEntry
	batchCommandsClients   []*batchCommandsClient
	tikvTransportLayerLoad uint64
	closed                 chan struct{}

	reqBuilder *batchCommandsBuilder

	// Notify rpcClient to check the idle flag
	idleNotify *uint32
	idleDetect *time.Timer

	fetchMoreTimer *time.Timer

	index uint32

	metrics batchConnMetrics
}

func newBatchConn(connCount, maxBatchSize uint, idleNotify *uint32) *batchConn {
	return &batchConn{
		batchCommandsCh:        make(chan *batchCommandsEntry, maxBatchSize),
		batchCommandsClients:   make([]*batchCommandsClient, 0, connCount),
		tikvTransportLayerLoad: 0,
		closed:                 make(chan struct{}),
		reqBuilder:             newBatchCommandsBuilder(maxBatchSize),
		idleNotify:             idleNotify,
		idleDetect:             time.NewTimer(idleTimeout),
	}
}

func (a *batchConn) initMetrics(target string) {
	a.metrics.pendingRequests = metrics.TiKVBatchPendingRequests.WithLabelValues(target)
	a.metrics.batchSize = metrics.TiKVBatchRequests.WithLabelValues(target)
	a.metrics.sendLoopWaitHeadDur = metrics.TiKVBatchSendLoopDuration.WithLabelValues(target, "wait-head")
	a.metrics.sendLoopWaitMoreDur = metrics.TiKVBatchSendLoopDuration.WithLabelValues(target, "wait-more")
	a.metrics.sendLoopSendDur = metrics.TiKVBatchSendLoopDuration.WithLabelValues(target, "send")
	a.metrics.recvLoopRecvDur = metrics.TiKVBatchRecvLoopDuration.WithLabelValues(target, "recv")
	a.metrics.recvLoopProcessDur = metrics.TiKVBatchRecvLoopDuration.WithLabelValues(target, "process")
	a.metrics.batchSendTailLat = metrics.TiKVBatchSendTailLatency.WithLabelValues(target)
	a.metrics.batchRecvTailLat = metrics.TiKVBatchRecvTailLatency.WithLabelValues(target)
	a.metrics.headArrivalInterval = metrics.TiKVBatchHeadArrivalInterval.WithLabelValues(target)
	a.metrics.batchMoreRequests = metrics.TiKVBatchMoreRequests.WithLabelValues(target)
	a.metrics.bestBatchSize = metrics.TiKVBatchBestSize.WithLabelValues(target)
}

func (a *batchConn) isIdle() bool {
	return atomic.LoadUint32(&a.idle) != 0
}

// fetchAllPendingRequests fetches all pending requests from the channel.
func (a *batchConn) fetchAllPendingRequests(maxBatchSize int) (headRecvTime time.Time, headArrivalInterval time.Duration) {
	// Block on the first element.
	latestReqStartTime := a.reqBuilder.latestReqStartTime
	var headEntry *batchCommandsEntry
	select {
	case headEntry = <-a.batchCommandsCh:
		if !a.idleDetect.Stop() {
			<-a.idleDetect.C
		}
		a.idleDetect.Reset(idleTimeout)
	case <-a.idleDetect.C:
		a.idleDetect.Reset(idleTimeout)
		atomic.AddUint32(&a.idle, 1)
		atomic.CompareAndSwapUint32(a.idleNotify, 0, 1)
		// This batchConn to be recycled
		return time.Now(), 0
	case <-a.closed:
		return time.Now(), 0
	}
	if headEntry == nil {
		return time.Now(), 0
	}
	headRecvTime = time.Now()
	if headEntry.start.After(latestReqStartTime) && !latestReqStartTime.IsZero() {
		headArrivalInterval = headEntry.start.Sub(latestReqStartTime)
	}
	a.reqBuilder.push(headEntry)

	// This loop is for trying best to collect more requests.
	for a.reqBuilder.len() < maxBatchSize {
		select {
		case entry := <-a.batchCommandsCh:
			if entry == nil {
				return
			}
			a.reqBuilder.push(entry)
		default:
			return
		}
	}
	return
}

// fetchMorePendingRequests fetches more pending requests from the channel.
func (a *batchConn) fetchMorePendingRequests(
	maxBatchSize int,
	batchWaitSize int,
	maxWaitTime time.Duration,
) {
	// Try to collect `batchWaitSize` requests, or wait `maxWaitTime`.
	if a.fetchMoreTimer == nil {
		a.fetchMoreTimer = time.NewTimer(maxWaitTime)
	} else {
		a.fetchMoreTimer.Reset(maxWaitTime)
	}
	for a.reqBuilder.len() < batchWaitSize {
		select {
		case entry := <-a.batchCommandsCh:
			if entry == nil {
				if !a.fetchMoreTimer.Stop() {
					<-a.fetchMoreTimer.C
				}
				return
			}
			a.reqBuilder.push(entry)
		case <-a.fetchMoreTimer.C:
			return
		}
	}
	if !a.fetchMoreTimer.Stop() {
		<-a.fetchMoreTimer.C
	}

	// Do an additional non-block try. Here we test the length with `maxBatchSize` instead
	// of `batchWaitSize` because trying best to fetch more requests is necessary so that
	// we can adjust the `batchWaitSize` dynamically.
	yielded := false
	for a.reqBuilder.len() < maxBatchSize {
		select {
		case entry := <-a.batchCommandsCh:
			if entry == nil {
				return
			}
			a.reqBuilder.push(entry)
		default:
			if yielded {
				return
			}
			// yield once to batch more requests.
			runtime.Gosched()
			yielded = true
		}
	}
}

const idleTimeout = 3 * time.Minute

var (
	// presetBatchPolicies defines a set of [turboBatchOptions] as batch policies.
	presetBatchPolicies = map[string]turboBatchOptions{
		config.BatchPolicyBasic:    {},
		config.BatchPolicyStandard: {V: turboBatchTimeBased, T: 0.0001, N: 5, W: 0.2, P: 0.8, Q: 0.8},
		config.BatchPolicyPositive: {V: turboBatchAlways, T: 0.0001},
	}
)

const (
	turboBatchAlways = iota
	turboBatchTimeBased
	turboBatchProbBased
)

// turboBatchOptions defines internal options for the [turboBatchTrigger].
type turboBatchOptions struct {
	// V determines the batch strategy: always(v=0), time-based(v=1), prob-based(v=2).
	V int `json:"v"`
	// N currently is used to determine the max arrival interval (n * t).
	N int `json:"n,omitempty"`
	// T is the max wait time for the batch.
	T float64 `json:"t,omitempty"`
	// W is used to adjust the `estArrivalInterval` or `estFetchMoreProb` dynamically.
	//   - time-based(v=1): estArrivalInterval = w*reqArrivalInterval + (1-w)*estArrivalInterval
	//   - prob-based(v=2): estFetchMoreProb = w*thisProb + (1-w)*estFetchMoreProb
	W float64 `json:"w,omitempty"`
	// P is used to determine whether to fetch more requests:
	//   - time-based(v=1): estArrivalInterval < p * t
	//   - prob-based(v=2): estFetchMoreProb > p
	P float64 `json:"p,omitempty"`
	// Q is used to adjust the `batchWaitSize` dynamically.
	Q float64 `json:"q,omitempty"`
}

// turboBatchTrigger is used to trigger the `fetchMorePendingRequests` dynamically according to the request arrival
// intervals. The option `v` indicates the strategy of triggering:
//
//   - turboBatchAlways: always fetch more requests.
//
//   - turboBatchTimeBased: fetch more requests if estArrivalInterval < p * t
//     where estArrivalInterval = w*reqArrivalInterval + (1-w)*estArrivalInterval
//     and reqArrivalInterval = min(reqArrivalInterval, n * t)
//
//   - turboBatchProbBased: fetch more requests if estFetchMoreProb > p
//     where estFetchMoreProb = w*thisProb + (1-w)*estFetchMoreProb
//     and thisProb = reqArrivalInterval < t ? 1 : 0
//
// The option `q` is used to adjust the `batchWaitSize` dynamically. If the fractional part of the `avgBatchWaitSize` is
// greater or equal to `q`, the `batchWaitSize` will be increased by 1.
type turboBatchTrigger struct {
	opts turboBatchOptions

	estFetchMoreProb   float64
	estArrivalInterval float64
	maxArrivalInterval float64
}

func newTurboBatchTriggerFromPolicy(policy string) (trigger turboBatchTrigger, ok bool) {
	if opts, found := presetBatchPolicies[policy]; found {
		return turboBatchTrigger{opts: opts}, true
	}
	rawOpts, _ := strings.CutPrefix(policy, config.BatchPolicyCustom)
	if err := json.Unmarshal([]byte(strings.TrimSpace(rawOpts)), &trigger.opts); err != nil {
		return turboBatchTrigger{opts: presetBatchPolicies[config.DefBatchPolicy]}, false
	}
	ok = true
	return
}

func (t *turboBatchTrigger) turboWaitSeconds() float64 {
	return t.opts.T
}

func (t *turboBatchTrigger) turboWaitTime() time.Duration {
	return time.Duration(t.opts.T * float64(time.Second))
}

func (t *turboBatchTrigger) needFetchMore(reqArrivalInterval time.Duration) bool {
	if t.opts.V == turboBatchTimeBased {
		thisArrivalInterval := reqArrivalInterval.Seconds()
		if t.maxArrivalInterval == 0 {
			t.maxArrivalInterval = t.turboWaitSeconds() * float64(t.opts.N)
		}
		if thisArrivalInterval > t.maxArrivalInterval {
			thisArrivalInterval = t.maxArrivalInterval
		}
		if t.estArrivalInterval == 0 {
			t.estArrivalInterval = thisArrivalInterval
		} else {
			t.estArrivalInterval = t.opts.W*thisArrivalInterval + (1-t.opts.W)*t.estArrivalInterval
		}
		return t.estArrivalInterval < t.turboWaitSeconds()*t.opts.P
	} else if t.opts.V == turboBatchProbBased {
		thisProb := .0
		if reqArrivalInterval.Seconds() < t.turboWaitSeconds() {
			thisProb = 1
		}
		t.estFetchMoreProb = t.opts.W*thisProb + (1-t.opts.W)*t.estFetchMoreProb
		return t.estFetchMoreProb > t.opts.P
	} else {
		return true
	}
}

func (t *turboBatchTrigger) preferredBatchWaitSize(avgBatchWaitSize float64, defBatchWaitSize int) int {
	if t.opts.V == turboBatchAlways {
		return defBatchWaitSize
	}
	n, m := math.Modf(avgBatchWaitSize)
	batchWaitSize := int(n)
	if m >= t.opts.Q {
		batchWaitSize++
	}
	return batchWaitSize
}

// BatchSendLoopPanicCounter is only used for testing.
var BatchSendLoopPanicCounter int64 = 0

var initBatchPolicyWarn sync.Once

func (a *batchConn) batchSendLoop(cfg config.TiKVClient) {
	defer func() {
		if r := recover(); r != nil {
			metrics.TiKVPanicCounter.WithLabelValues(metrics.LabelBatchSendLoop).Inc()
			logutil.BgLogger().Error("batchSendLoop",
				zap.Any("r", r),
				zap.Stack("stack"))
			atomic.AddInt64(&BatchSendLoopPanicCounter, 1)
			logutil.BgLogger().Info("restart batchSendLoop", zap.Int64("count", atomic.LoadInt64(&BatchSendLoopPanicCounter)))
			go a.batchSendLoop(cfg)
		}
	}()

	trigger, ok := newTurboBatchTriggerFromPolicy(cfg.BatchPolicy)
	if !ok {
		initBatchPolicyWarn.Do(func() {
			logutil.BgLogger().Warn("fallback to default batch policy due to invalid value", zap.String("value", cfg.BatchPolicy))
		})
	}
	turboBatchWaitTime := trigger.turboWaitTime()

	avgBatchWaitSize := float64(cfg.BatchWaitSize)
	for {
		sendLoopStartTime := time.Now()
		a.reqBuilder.reset()

		headRecvTime, headArrivalInterval := a.fetchAllPendingRequests(int(cfg.MaxBatchSize))
		if a.reqBuilder.len() == 0 {
			// the conn is closed or recycled.
			return
		}

		// curl -X PUT -d 'return(true)' http://0.0.0.0:10080/fail/tikvclient/mockBlockOnBatchClient
		if val, err := util.EvalFailpoint("mockBlockOnBatchClient"); err == nil {
			if val.(bool) {
				time.Sleep(1 * time.Hour)
			}
		}

		if batchSize := a.reqBuilder.len(); batchSize < int(cfg.MaxBatchSize) {
			if cfg.MaxBatchWaitTime > 0 && atomic.LoadUint64(&a.tikvTransportLayerLoad) > uint64(cfg.OverloadThreshold) {
				// If the target TiKV is overload, wait a while to collect more requests.
				metrics.TiKVBatchWaitOverLoad.Inc()
				a.fetchMorePendingRequests(int(cfg.MaxBatchSize), int(cfg.BatchWaitSize), cfg.MaxBatchWaitTime)
			} else if turboBatchWaitTime > 0 && headArrivalInterval > 0 && trigger.needFetchMore(headArrivalInterval) {
				batchWaitSize := trigger.preferredBatchWaitSize(avgBatchWaitSize, int(cfg.BatchWaitSize))
				a.fetchMorePendingRequests(int(cfg.MaxBatchSize), batchWaitSize, turboBatchWaitTime)
				a.metrics.batchMoreRequests.Observe(float64(a.reqBuilder.len() - batchSize))
			}
		}
		length := a.reqBuilder.len()
		avgBatchWaitSize = 0.2*float64(length) + 0.8*avgBatchWaitSize
		a.metrics.pendingRequests.Observe(float64(len(a.batchCommandsCh) + length))
		a.metrics.bestBatchSize.Observe(avgBatchWaitSize)
		a.metrics.headArrivalInterval.Observe(headArrivalInterval.Seconds())
		a.metrics.sendLoopWaitHeadDur.Observe(headRecvTime.Sub(sendLoopStartTime).Seconds())
		a.metrics.sendLoopWaitMoreDur.Observe(time.Since(sendLoopStartTime).Seconds())

		a.getClientAndSend()

		sendLoopEndTime := time.Now()
		a.metrics.sendLoopSendDur.Observe(sendLoopEndTime.Sub(sendLoopStartTime).Seconds())
		if dur := sendLoopEndTime.Sub(headRecvTime); dur > batchSendTailLatThreshold {
			a.metrics.batchSendTailLat.Observe(dur.Seconds())
		}
	}
}

const (
	SendFailedReasonNoAvailableLimit   = "concurrency limit exceeded"
	SendFailedReasonTryLockForSendFail = "tryLockForSend fail"
)

func (a *batchConn) getClientAndSend() {
	if val, err := util.EvalFailpoint("mockBatchClientSendDelay"); err == nil {
		if timeout, ok := val.(int); ok && timeout > 0 {
			time.Sleep(time.Duration(timeout * int(time.Millisecond)))
		}
	}

	// Choose a connection by round-robbin.
	var (
		cli    *batchCommandsClient
		target string
	)
	reasons := make([]string, 0)
	hasHighPriorityTask := a.reqBuilder.hasHighPriorityTask()
	for i := 0; i < len(a.batchCommandsClients); i++ {
		a.index = (a.index + 1) % uint32(len(a.batchCommandsClients))
		target = a.batchCommandsClients[a.index].target
		// The lock protects the batchCommandsClient from been closed while it's in use.
		c := a.batchCommandsClients[a.index]
		if hasHighPriorityTask || c.available() > 0 {
			if c.tryLockForSend() {
				cli = c
				break
			} else {
				reasons = append(reasons, SendFailedReasonTryLockForSendFail)
			}
		} else {
			reasons = append(reasons, SendFailedReasonNoAvailableLimit)
		}
	}
	if cli == nil {
		logutil.BgLogger().Info("no available connections", zap.String("target", target), zap.Any("reasons", reasons))
		metrics.TiKVNoAvailableConnectionCounter.Inc()
		if config.GetGlobalConfig().TiKVClient.MaxConcurrencyRequestLimit == config.DefMaxConcurrencyRequestLimit {
			// Only cancel requests when MaxConcurrencyRequestLimit feature is not enabled, to be compatible with the behavior of older versions.
			// TODO: But when MaxConcurrencyRequestLimit feature is enabled, the requests won't be canceled and will wait until timeout.
			// This behavior may not be reasonable, as the timeout is usually 40s or 60s, which is too long to retry in time.
			a.reqBuilder.cancel(errors.New("no available connections"))
		}
		return
	}
	defer cli.unlockForSend()
	available := cli.available()
	reqSendTime := time.Now()
	batch := 0
	req, forwardingReqs := a.reqBuilder.buildWithLimit(available, func(id uint64, e *batchCommandsEntry) {
		cli.batched.Store(id, e)
		cli.sent.Add(1)
		atomic.StoreInt64(&e.sendLat, int64(reqSendTime.Sub(e.start)))
		if trace.IsEnabled() {
			trace.Log(e.ctx, "rpc", "send")
		}
	})
	if req != nil {
		batch += len(req.RequestIds)
		cli.send("", req)
	}
	for forwardedHost, req := range forwardingReqs {
		batch += len(req.RequestIds)
		cli.send(forwardedHost, req)
	}
	if batch > 0 {
		a.metrics.batchSize.Observe(float64(batch))
	}
}

type tryLock struct {
	*sync.Cond
	reCreating bool
}

func (l *tryLock) tryLockForSend() bool {
	l.L.Lock()
	if l.reCreating {
		l.L.Unlock()
		return false
	}
	return true
}

func (l *tryLock) unlockForSend() {
	l.L.Unlock()
}

func (l *tryLock) lockForRecreate() {
	l.L.Lock()
	for l.reCreating {
		l.Wait()
	}
	l.reCreating = true
	l.L.Unlock()
}

func (l *tryLock) unlockForRecreate() {
	l.L.Lock()
	l.reCreating = false
	l.Broadcast()
	l.L.Unlock()
}

type batchCommandsStream struct {
	tikvpb.Tikv_BatchCommandsClient
	forwardedHost string
}

func (s *batchCommandsStream) recv() (resp *tikvpb.BatchCommandsResponse, err error) {
	defer func() {
		if r := recover(); r != nil {
			metrics.TiKVPanicCounter.WithLabelValues(metrics.LabelBatchRecvLoop).Inc()
			logutil.BgLogger().Error("batchCommandsClient.recv panic",
				zap.Any("r", r),
				zap.Stack("stack"))
			err = errors.New("batch conn recv paniced")
		}
	}()
	if _, err := util.EvalFailpoint("gotErrorInRecvLoop"); err == nil {
		return nil, errors.New("injected error in batchRecvLoop")
	}
	// When `conn.Close()` is called, `client.Recv()` will return an error.
	resp, err = s.Recv()
	return
}

// recreate creates a new BatchCommands stream. The conn should be ready for work.
func (s *batchCommandsStream) recreate(conn *grpc.ClientConn) error {
	tikvClient := tikvpb.NewTikvClient(conn)
	ctx := context.TODO()
	// Set metadata for forwarding stream.
	if s.forwardedHost != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, forwardMetadataKey, s.forwardedHost)
	}
	streamClient, err := tikvClient.BatchCommands(ctx)
	if err != nil {
		return errors.WithStack(err)
	}
	s.Tikv_BatchCommandsClient = streamClient
	return nil
}

type batchCommandsClient struct {
	// The target host.
	target string

	conn *grpc.ClientConn
	// client and forwardedClients are protected by tryLock.
	//
	// client is the stream that needn't forwarding.
	client *batchCommandsStream
	// TiDB uses [gRPC-metadata](https://github.com/grpc/grpc-go/blob/master/Documentation/grpc-metadata.md) to
	// indicate a request needs forwarding. gRPC doesn't support setting a metadata for each request in a stream,
	// so we need to create a stream for each forwarded host.
	//
	// forwardedClients are clients that need forwarding. It's a map that maps forwarded hosts to streams
	forwardedClients map[string]*batchCommandsStream
	batched          sync.Map

	tikvClientCfg config.TiKVClient
	tikvLoad      *uint64
	dialTimeout   time.Duration

	// Increased in each reconnection.
	// It's used to prevent the connection from reconnecting multiple times
	// due to one failure because there may be more than 1 `batchRecvLoop`s.
	epoch uint64
	// closed indicates the batch client is closed explicitly or not.
	closed int32
	// tryLock protects client when re-create the streaming.
	tryLock

	// sent is the number of the requests are processed by tikv server.
	sent atomic.Int64
	// maxConcurrencyRequestLimit is the max allowed number of requests to be sent the tikv
	maxConcurrencyRequestLimit atomic.Int64

	// eventListener is the listener set by external code to observe some events in the client. It's stored in a atomic
	// pointer to make setting thread-safe.
	eventListener *atomic.Pointer[ClientEventListener]

	metrics *batchConnMetrics
}

func (c *batchCommandsClient) isStopped() bool {
	return atomic.LoadInt32(&c.closed) != 0
}

func (c *batchCommandsClient) available() int64 {
	limit := c.maxConcurrencyRequestLimit.Load()
	sent := c.sent.Load()
	//  The `sent` could be less than 0, see https://github.com/tikv/client-go/issues/1225 for details.
	if sent > 0 {
		if limit > sent {
			return limit - sent
		}
		return 0
	}
	return limit
}

func (c *batchCommandsClient) send(forwardedHost string, req *tikvpb.BatchCommandsRequest) {
	err := c.initBatchClient(forwardedHost)
	if err != nil {
		logutil.BgLogger().Warn(
			"init create streaming fail",
			zap.String("target", c.target),
			zap.String("forwardedHost", forwardedHost),
			zap.Error(err),
		)
		c.failRequestsByIDs(err, req.RequestIds) // fast fail requests.
		return
	}

	client := c.client
	if forwardedHost != "" {
		client = c.forwardedClients[forwardedHost]
	}
	if err := client.Send(req); err != nil {
		logutil.BgLogger().Info(
			"sending batch commands meets error",
			zap.String("target", c.target),
			zap.String("forwardedHost", forwardedHost),
			zap.Uint64s("requestIDs", req.RequestIds),
			zap.Error(err),
		)
		c.failRequestsByIDs(err, req.RequestIds) // fast fail requests.
	}
}

// `failPendingRequests` must be called in locked contexts in order to avoid double closing channels.
// when enable-forwarding is true, the `forwardedHost` maybe not empty.
// failPendingRequests fails all pending requests which req.forwardedHost equals to forwardedHost parameter.
// Why need check `forwardedHost`? Here is an example, when enable-forwarding is true, and this client has network issue with store1:
//   - some requests are sent to store1 with forwarding, such as forwardedHost is store2, those requests will succeed.
//   - some requests are sent to store1 without forwarding, and may fail then `failPendingRequests` would be called,
//     if we don't check `forwardedHost` and fail all pending requests, the requests with forwarding will be failed too. this may cause some issue:
//     1. data race. see https://github.com/tikv/client-go/issues/1222 and TestRandomRestartStoreAndForwarding.
//     2. panic which cause by `send on closed channel`, since failPendingRequests will close the entry.res channel,
//     but in another batchRecvLoop goroutine,  it may receive the response from forwardedHost store2 and try to send the response to entry.res channel,
//     then panic by send on closed channel.
func (c *batchCommandsClient) failPendingRequests(err error, forwardedHost string) {
	util.EvalFailpoint("panicInFailPendingRequests")
	c.batched.Range(func(key, value interface{}) bool {
		id, _ := key.(uint64)
		entry, _ := value.(*batchCommandsEntry)
		if entry.forwardedHost == forwardedHost {
			c.failRequest(err, id, entry)
		}
		return true
	})
}

// failAsyncRequestsOnClose fails all async requests when the client is closed.
func (c *batchCommandsClient) failAsyncRequestsOnClose() {
	err := errors.New("batch client closed")
	c.batched.Range(func(key, value interface{}) bool {
		id, _ := key.(uint64)
		entry, _ := value.(*batchCommandsEntry)
		if entry.async() {
			c.failRequest(err, id, entry)
		}
		return true
	})
}

// failRequestsByIDs fails requests by requestID.
func (c *batchCommandsClient) failRequestsByIDs(err error, requestIDs []uint64) {
	for _, requestID := range requestIDs {
		value, ok := c.batched.Load(requestID)
		if !ok {
			continue
		}
		c.failRequest(err, requestID, value.(*batchCommandsEntry))
	}
}

func (c *batchCommandsClient) failRequest(err error, requestID uint64, entry *batchCommandsEntry) {
	c.batched.Delete(requestID)
	c.sent.Add(-1)
	entry.error(err)
}

func (c *batchCommandsClient) waitConnReady() (err error) {
	state := c.conn.GetState()
	if state == connectivity.Ready {
		return
	}
	// Trigger idle connection to reconnection
	// Put it outside loop to avoid unnecessary reconnecting.
	if state == connectivity.Idle {
		c.conn.Connect()
	}
	start := time.Now()
	defer func() {
		metrics.TiKVBatchClientWaitEstablish.Observe(time.Since(start).Seconds())
	}()
	dialCtx, cancel := context.WithTimeout(context.Background(), c.dialTimeout)
	for {
		s := c.conn.GetState()
		if s == connectivity.Ready {
			cancel()
			break
		}
		if !c.conn.WaitForStateChange(dialCtx, s) {
			cancel()
			err = dialCtx.Err()
			return
		}
	}
	return
}

func (c *batchCommandsClient) recreateStreamingClientOnce(streamClient *batchCommandsStream) error {
	err := c.waitConnReady()
	// Re-establish a application layer stream. TCP layer is handled by gRPC.
	if err == nil {
		err := streamClient.recreate(c.conn)
		if err == nil {
			logutil.BgLogger().Info(
				"batchRecvLoop re-create streaming success",
				zap.String("target", c.target),
				zap.String("forwardedHost", streamClient.forwardedHost),
			)
			return nil
		}
	}
	logutil.BgLogger().Info(
		"batchRecvLoop re-create streaming fail",
		zap.String("target", c.target),
		zap.String("forwardedHost", streamClient.forwardedHost),
		zap.Error(err),
	)
	return err
}

func (c *batchCommandsClient) batchRecvLoop(cfg config.TiKVClient, tikvTransportLayerLoad *uint64, connMetrics *batchConnMetrics, streamClient *batchCommandsStream) {
	defer func() {
		if r := recover(); r != nil {
			metrics.TiKVPanicCounter.WithLabelValues(metrics.LabelBatchRecvLoop).Inc()
			logutil.BgLogger().Error("batchRecvLoop",
				zap.Any("r", r),
				zap.Stack("stack"))
			logutil.BgLogger().Info("restart batchRecvLoop")
			go c.batchRecvLoop(cfg, tikvTransportLayerLoad, connMetrics, streamClient)
		} else {
			c.failAsyncRequestsOnClose()
		}
	}()

	epoch := atomic.LoadUint64(&c.epoch)
	for {
		recvLoopStartTime := time.Now()
		resp, err := streamClient.recv()
		respRecvTime := time.Now()
		recvDur := respRecvTime.Sub(recvLoopStartTime)
		connMetrics.recvLoopRecvDur.Observe(recvDur.Seconds())
		if recvDur > batchRecvTailLatThreshold {
			c.metrics.batchRecvTailLat.Observe(recvDur.Seconds())
		}
		if err != nil {
			if c.isStopped() {
				return
			}
			logutil.BgLogger().Debug(
				"batchRecvLoop fails when receiving, needs to reconnect",
				zap.String("target", c.target),
				zap.String("forwardedHost", streamClient.forwardedHost),
				zap.Error(err),
			)

			now := time.Now()
			if stopped := c.recreateStreamingClient(err, streamClient, &epoch); stopped {
				return
			}
			metrics.TiKVBatchClientUnavailable.Observe(time.Since(now).Seconds())
			continue
		}

		if resp.GetHealthFeedback() != nil {
			if val, err := util.EvalFailpoint("injectHealthFeedbackSlowScore"); err == nil {
				v, ok := val.(int)
				if !ok || v < 0 || v > 100 {
					panic(fmt.Sprintf("invalid injection in failpoint injectHealthFeedbackSlowScore: %+q", v))
				}
				resp.GetHealthFeedback().SlowScore = int32(v)
			}
			c.onHealthFeedback(resp.GetHealthFeedback())
		}

		responses := resp.GetResponses()
		for i, requestID := range resp.GetRequestIds() {
			value, ok := c.batched.Load(requestID)
			if !ok {
				// this maybe caused by batchCommandsClient#send meets ambiguous error that request has be sent to TiKV but still report a error.
				// then TiKV will send response back though stream and reach here.
				logutil.BgLogger().Warn("batchRecvLoop receives outdated response", zap.Uint64("requestID", requestID), zap.String("forwardedHost", streamClient.forwardedHost))
				continue
			}
			entry := value.(*batchCommandsEntry)

			atomic.StoreInt64(&entry.recvLat, int64(respRecvTime.Sub(entry.start)))
			if trace.IsEnabled() {
				trace.Log(entry.ctx, "rpc", "received")
			}
			logutil.Eventf(entry.ctx, "receive %T response with other %d batched requests from %s", responses[i].GetCmd(), len(responses), c.target)
			if atomic.LoadInt32(&entry.canceled) == 0 {
				// Put the response only if the request is not canceled.
				entry.response(responses[i])
			}
			c.batched.Delete(requestID)
			c.sent.Add(-1)
		}

		transportLayerLoad := resp.GetTransportLayerLoad()
		if transportLayerLoad > 0 && cfg.MaxBatchWaitTime > 0 {
			// We need to consider TiKV load only if batch-wait strategy is enabled.
			atomic.StoreUint64(tikvTransportLayerLoad, transportLayerLoad)
		}
		connMetrics.recvLoopProcessDur.Observe(time.Since(recvLoopStartTime).Seconds())
	}
}

func (c *batchCommandsClient) onHealthFeedback(feedback *kvrpcpb.HealthFeedback) {
	if h := c.eventListener.Load(); h != nil {
		(*h).OnHealthFeedback(feedback)
	}
}

func (c *batchCommandsClient) recreateStreamingClient(err error, streamClient *batchCommandsStream, epoch *uint64) (stopped bool) {
	// Forbids the batchSendLoop using the old client and
	// blocks other streams trying to recreate.
	c.lockForRecreate()
	defer c.unlockForRecreate()

	// Each batchCommandsStream has a batchRecvLoop. There is only one stream waiting for
	// the connection ready in every epoch to prevent the connection from reconnecting
	// multiple times due to one failure.
	//
	// Check it in the locked scope to prevent the stream which gets the token from
	// reconnecting lately, i.e.
	// goroutine 1       | goroutine 2
	// CAS success       |
	//                   | CAS failure
	//                   | lockForRecreate
	//                   | recreate error
	//                   | unlockForRecreate
	// lockForRecreate   |
	// waitConnReady     |
	// recreate          |
	// unlockForRecreate |
	waitConnReady := atomic.CompareAndSwapUint64(&c.epoch, *epoch, *epoch+1)
	if !waitConnReady {
		*epoch = atomic.LoadUint64(&c.epoch)
		if err := streamClient.recreate(c.conn); err != nil {
			logutil.BgLogger().Info(
				"batchRecvLoop re-create streaming fail",
				zap.String("target", c.target),
				zap.String("forwardedHost", streamClient.forwardedHost),
				zap.Error(err),
			)
		}
		return c.isStopped()
	}
	*epoch++

	c.failPendingRequests(err, streamClient.forwardedHost) // fail all pending requests.
	b := retry.NewBackofferWithVars(context.Background(), math.MaxInt32, nil)
	for { // try to re-create the streaming in the loop.
		if c.isStopped() {
			return true
		}
		err1 := c.recreateStreamingClientOnce(streamClient)
		if err1 == nil {
			break
		}

		err2 := b.Backoff(retry.BoTiKVRPC, err1)
		// As timeout is set to math.MaxUint32, err2 should always be nil.
		// This line is added to make the 'make errcheck' pass.
		tikverr.Log(err2)
	}
	return false
}

func (c *batchCommandsClient) newBatchStream(forwardedHost string) (*batchCommandsStream, error) {
	batchStream := &batchCommandsStream{forwardedHost: forwardedHost}
	if err := batchStream.recreate(c.conn); err != nil {
		return nil, err
	}
	return batchStream, nil
}

func (c *batchCommandsClient) initBatchClient(forwardedHost string) error {
	if forwardedHost == "" && c.client != nil {
		return nil
	}
	if _, ok := c.forwardedClients[forwardedHost]; ok {
		return nil
	}

	if err := c.waitConnReady(); err != nil {
		return err
	}

	streamClient, err := c.newBatchStream(forwardedHost)
	if err != nil {
		return err
	}
	if forwardedHost == "" {
		c.client = streamClient
	} else {
		c.forwardedClients[forwardedHost] = streamClient
	}
	go c.batchRecvLoop(c.tikvClientCfg, c.tikvLoad, c.metrics, streamClient)
	return nil
}

func (a *batchConn) Close() {
	// Close all batchRecvLoop.
	for _, c := range a.batchCommandsClients {
		// After connections are closed, `batchRecvLoop`s will check the flag.
		atomic.StoreInt32(&c.closed, 1)
	}
	// Don't close(batchCommandsCh) because when Close() is called, someone maybe
	// calling SendRequest and writing batchCommandsCh, if we close it here the
	// writing goroutine will panic.
	close(a.closed)
}

func sendBatchRequest(
	ctx context.Context,
	addr string,
	forwardedHost string,
	batchConn *batchConn,
	req *tikvpb.BatchCommandsRequest_Request,
	timeout time.Duration,
	priority uint64,
) (*tikvrpc.Response, error) {
	entry := &batchCommandsEntry{
		ctx:           ctx,
		req:           req,
		res:           make(chan *tikvpb.BatchCommandsResponse_Response, 1),
		forwardedHost: forwardedHost,
		canceled:      0,
		err:           nil,
		pri:           priority,
		start:         time.Now(),
	}
	timer := time.NewTimer(timeout)
	defer func() {
		timer.Stop()
		if sendLat := atomic.LoadInt64(&entry.sendLat); sendLat > 0 {
			metrics.BatchRequestDurationSend.Observe(time.Duration(sendLat).Seconds())
		}
		if recvLat := atomic.LoadInt64(&entry.recvLat); recvLat > 0 {
			metrics.BatchRequestDurationRecv.Observe(time.Duration(recvLat).Seconds())
		}
		metrics.BatchRequestDurationDone.Observe(time.Since(entry.start).Seconds())
	}()

	select {
	case batchConn.batchCommandsCh <- entry:
	case <-ctx.Done():
		logutil.Logger(ctx).Debug("send request is cancelled",
			zap.String("to", addr), zap.String("cause", ctx.Err().Error()))
		return nil, errors.WithStack(ctx.Err())
	case <-batchConn.closed:
		logutil.Logger(ctx).Debug("send request is cancelled (batchConn closed)", zap.String("to", addr))
		return nil, errors.New("batchConn closed")
	case <-timer.C:
		return nil, errors.WithMessage(context.DeadlineExceeded, "wait sendLoop")
	}

	select {
	case res, ok := <-entry.res:
		if !ok {
			return nil, errors.WithStack(entry.err)
		}
		return tikvrpc.FromBatchCommandsResponse(res)
	case <-ctx.Done():
		atomic.StoreInt32(&entry.canceled, 1)
		logutil.Logger(ctx).Debug("wait response is cancelled",
			zap.String("to", addr), zap.String("cause", ctx.Err().Error()))
		return nil, errors.WithStack(ctx.Err())
	case <-batchConn.closed:
		atomic.StoreInt32(&entry.canceled, 1)
		logutil.Logger(ctx).Debug("wait response is cancelled (batchConn closed)", zap.String("to", addr))
		return nil, errors.New("batchConn closed")
	case <-timer.C:
		atomic.StoreInt32(&entry.canceled, 1)
		reason := fmt.Sprintf("wait recvLoop timeout, timeout:%s", timeout)
		if sendLat := atomic.LoadInt64(&entry.sendLat); sendLat > 0 {
			reason += fmt.Sprintf(", send:%s", util.FormatDuration(time.Duration(sendLat)))
			if recvLat := atomic.LoadInt64(&entry.recvLat); recvLat > 0 {
				reason += fmt.Sprintf(", recv:%s", util.FormatDuration(time.Duration(recvLat-sendLat)))
			}
		}
		return nil, errors.WithMessage(context.DeadlineExceeded, reason)
	}
}
