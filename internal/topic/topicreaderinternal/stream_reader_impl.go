package topicreaderinternal

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"runtime/pprof"
	"sync/atomic"
	"time"

	"github.com/ydb-platform/ydb-go-sdk/v3/credentials"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/background"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/grpcwrapper/rawtopic/rawtopiccommon"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/grpcwrapper/rawtopic/rawtopicreader"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/xcontext"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/xerrors"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/xsync"
	"github.com/ydb-platform/ydb-go-sdk/v3/trace"
)

var (
	errPartitionSessionStoppedByServer = xerrors.Wrap(errors.New("ydb: topic partition session stopped by server"))
	errPartitionSessionStoppedBySDK    = xerrors.Wrap(errors.New("ydb: topic partition session stopped by sdk"))
	errCommitSessionFromOtherReader    = xerrors.Wrap(errors.New("ydb: commit with session from other reader"))
	errCommitWithNilPartitionSession   = xerrors.Wrap(errors.New("ydb: commit with nil partition session"))
)

type partitionSessionID = rawtopicreader.PartitionSessionID

type topicStreamReaderImpl struct {
	cfg    topicStreamReaderConfig
	ctx    context.Context
	cancel xcontext.CancelErrFunc

	freeBytes                 chan int
	atomicRestBufferSizeBytes int64
	sessionController         partitionSessionStorage
	backgroundWorkers         background.Worker

	rawMessagesFromBuffer chan rawtopicreader.ServerMessage

	batcher   *batcher
	committer *committer

	stream           RawTopicReaderStream
	readConnectionID string

	m       xsync.RWMutex
	err     error
	started bool
	closed  bool
}

type topicStreamReaderConfig struct {
	CommitterBatchTimeLag           time.Duration
	CommitterBatchCounterTrigger    int
	BaseContext                     context.Context
	BufferSizeProtoBytes            int
	Cred                            credentials.Credentials
	CredUpdateInterval              time.Duration
	Consumer                        string
	ReadSelectors                   []PublicReadSelector
	Tracer                          trace.Topic
	GetPartitionStartOffsetCallback PublicGetPartitionStartOffsetFunc
	CommitMode                      PublicCommitMode
	Decoders                        decoderMap
}

func newTopicStreamReaderConfig() topicStreamReaderConfig {
	return topicStreamReaderConfig{
		BaseContext:           context.Background(),
		BufferSizeProtoBytes:  1024 * 1024,
		Cred:                  credentials.NewAnonymousCredentials(),
		CredUpdateInterval:    time.Hour,
		CommitMode:            CommitModeAsync,
		CommitterBatchTimeLag: time.Second,
		Decoders:              newDecoderMap(),
	}
}

func (cfg *topicStreamReaderConfig) initMessage() *rawtopicreader.InitRequest {
	res := &rawtopicreader.InitRequest{
		Consumer: cfg.Consumer,
	}

	res.TopicsReadSettings = make([]rawtopicreader.TopicReadSettings, len(cfg.ReadSelectors))
	for i, selector := range cfg.ReadSelectors {
		settings := &res.TopicsReadSettings[i]
		settings.Path = selector.Path
		settings.PartitionsID = selector.Partitions
		if !selector.ReadFrom.IsZero() {
			settings.ReadFrom.HasValue = true
			settings.ReadFrom.Value = selector.ReadFrom
		}
		if selector.MaxTimeLag != 0 {
			settings.MaxLag.HasValue = true
			settings.MaxLag.Value = selector.MaxTimeLag
		}
	}

	return res
}

func newTopicStreamReader(
	stream RawTopicReaderStream,
	cfg topicStreamReaderConfig,
) (_ *topicStreamReaderImpl, err error) {
	defer func() {
		if err != nil {
			_ = stream.CloseSend()
		}
	}()

	reader := newTopicStreamReaderStopped(stream, cfg)
	if err = reader.initSession(); err != nil {
		return nil, err
	}
	if err = reader.startLoops(); err != nil {
		return nil, err
	}

	return reader, nil
}

func newTopicStreamReaderStopped(
	stream RawTopicReaderStream,
	cfg topicStreamReaderConfig,
) *topicStreamReaderImpl {
	labeledContext := pprof.WithLabels(cfg.BaseContext, pprof.Labels("base-context", "topic-stream-reader"))
	stopPump, cancel := xcontext.WithErrCancel(labeledContext)

	readerConnectionID, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt64))
	if err != nil {
		readerConnectionID = big.NewInt(-1)
	}

	res := &topicStreamReaderImpl{
		cfg:                   cfg,
		ctx:                   stopPump,
		freeBytes:             make(chan int, 1),
		stream:                &syncedStream{stream: stream},
		cancel:                cancel,
		batcher:               newBatcher(),
		backgroundWorkers:     *background.NewWorker(stopPump),
		readConnectionID:      "preinitID-" + readerConnectionID.String(),
		rawMessagesFromBuffer: make(chan rawtopicreader.ServerMessage, 1),
	}

	res.committer = newCommitter(cfg.Tracer, labeledContext, cfg.CommitMode, res.send)
	res.committer.BufferTimeLagTrigger = cfg.CommitterBatchTimeLag
	res.committer.BufferCountTrigger = cfg.CommitterBatchCounterTrigger
	res.sessionController.init()
	res.freeBytes <- cfg.BufferSizeProtoBytes
	return res
}

func (r *topicStreamReaderImpl) ReadMessageBatch(
	ctx context.Context,
	opts ReadMessageBatchOptions,
) (batch *PublicBatch, err error) {
	onDone := trace.TopicOnReaderReadMessages(
		r.cfg.Tracer,
		ctx,
		opts.MinCount,
		opts.MaxCount,
		r.getRestBufferBytes(),
	)
	defer func() {
		if batch == nil {
			onDone(0, "", -1, -1, -1, -1, r.getRestBufferBytes(), err)
		} else {
			onDone(
				len(batch.Messages),
				batch.Topic(),
				batch.PartitionID(),
				batch.partitionSession().partitionSessionID.ToInt64(),
				batch.commitRange.commitOffsetStart.ToInt64(),
				batch.commitRange.commitOffsetEnd.ToInt64(),
				r.getRestBufferBytes(),
				err,
			)
		}
	}()

	if err = ctx.Err(); err != nil {
		return nil, err
	}

	defer func() {
		if err == nil {
			r.freeBufferFromMessages(batch)
		}
	}()

	return r.consumeMessagesUntilBatch(ctx, opts)
}

func (r *topicStreamReaderImpl) consumeMessagesUntilBatch(
	ctx context.Context,
	opts ReadMessageBatchOptions,
) (*PublicBatch, error) {
	for {
		item, err := r.batcher.Pop(ctx, opts.batcherGetOptions)
		if err != nil {
			return nil, err
		}

		switch {
		case item.IsBatch():
			return item.Batch, nil
		case item.IsRawMessage():
			r.sendRawMessageToChannelUnblocked(item.RawMessage)
		default:
			return nil, xerrors.WithStackTrace(fmt.Errorf("ydb: unexpected item type from batcher: %#v", item))
		}
	}
}

func (r *topicStreamReaderImpl) sendRawMessageToChannelUnblocked(msg rawtopicreader.ServerMessage) {
	select {
	case r.rawMessagesFromBuffer <- msg:
		return
	default:
		// send in goroutine, without block caller
		r.backgroundWorkers.Start("sendMessageToRawChannel", func(ctx context.Context) {
			select {
			case r.rawMessagesFromBuffer <- msg:
			case <-ctx.Done():
			}
		})
	}
}

func (r *topicStreamReaderImpl) consumeRawMessageFromBuffer(ctx context.Context) {
	doneChan := ctx.Done()

	for {
		var msg rawtopicreader.ServerMessage
		select {
		case <-doneChan:
			return
		case msg = <-r.rawMessagesFromBuffer:
			// pass
		}

		switch m := msg.(type) {
		case *rawtopicreader.StartPartitionSessionRequest:
			if err := r.onStartPartitionSessionRequestFromBuffer(m); err != nil {
				_ = r.CloseWithError(ctx, err)
				return
			}
		case *rawtopicreader.StopPartitionSessionRequest:
			if err := r.onStopPartitionSessionRequestFromBuffer(m); err != nil {
				_ = r.CloseWithError(ctx, xerrors.WithStackTrace(
					fmt.Errorf("ydb: unexpected error on stop partition handler: %w", err),
				))
				return
			}
		case *rawtopicreader.PartitionSessionStatusResponse:
			r.onPartitionSessionStatusResponseFromBuffer(ctx, m)
		default:
			_ = r.CloseWithError(ctx, xerrors.WithStackTrace(
				fmt.Errorf("ydb: unexpected server message from buffer: %v", reflect.TypeOf(msg))),
			)
		}
	}
}

func (r *topicStreamReaderImpl) onStopPartitionSessionRequestFromBuffer(
	msg *rawtopicreader.StopPartitionSessionRequest,
) (err error) {
	session, err := r.sessionController.Get(msg.PartitionSessionID)
	if err != nil {
		return err
	}

	onDone := trace.TopicOnReaderPartitionReadStopResponse(
		r.cfg.Tracer,
		r.readConnectionID,
		session.Context(),
		session.Topic,
		session.PartitionID,
		session.partitionSessionID.ToInt64(),
		msg.CommittedOffset.ToInt64(),
		msg.Graceful,
	)
	defer func() {
		onDone(err)
	}()

	if msg.Graceful {
		session.Close(errPartitionSessionStoppedBySDK)
		resp := &rawtopicreader.StopPartitionSessionResponse{
			PartitionSessionID: session.partitionSessionID,
		}
		if err = r.send(resp); err != nil {
			return err
		}
	}

	if _, err = r.sessionController.Remove(session.partitionSessionID); err != nil {
		if msg.Graceful {
			return err
		} else { //nolint:revive,staticcheck
			// double message with graceful=false is ok.
			// It may be received after message with graceful=true and session was removed while process that.

			// pass
		}
	}

	return nil
}

func (r *topicStreamReaderImpl) onPartitionSessionStatusResponseFromBuffer(
	ctx context.Context,
	m *rawtopicreader.PartitionSessionStatusResponse,
) {
	panic("not implemented")
}

func (r *topicStreamReaderImpl) Commit(ctx context.Context, commitRange commitRange) (err error) {
	if commitRange.partitionSession == nil {
		return xerrors.WithStackTrace(errCommitWithNilPartitionSession)
	}

	session := commitRange.partitionSession
	onDone := trace.TopicOnReaderCommit(
		r.cfg.Tracer,
		ctx,
		session.Topic,
		session.PartitionID,
		session.partitionSessionID.ToInt64(),
		commitRange.commitOffsetStart.ToInt64(),
		commitRange.commitOffsetEnd.ToInt64(),
	)
	defer func() {
		onDone(err)
	}()

	if err = r.checkCommitRange(commitRange); err != nil {
		return err
	}
	return r.committer.Commit(ctx, commitRange)
}

func (r *topicStreamReaderImpl) checkCommitRange(commitRange commitRange) error {
	session := commitRange.partitionSession

	if session == nil {
		return xerrors.WithStackTrace(errCommitWithNilPartitionSession)
	}

	if session.Context().Err() != nil {
		return xerrors.WithStackTrace(fmt.Errorf("ydb: commit error: %w", errPartitionSessionStoppedByServer))
	}

	ownSession, err := r.sessionController.Get(session.partitionSessionID)
	if err != nil || session != ownSession {
		return xerrors.WithStackTrace(errCommitSessionFromOtherReader)
	}

	return nil
}

func (r *topicStreamReaderImpl) send(msg rawtopicreader.ClientMessage) error {
	err := r.stream.Send(msg)
	if err != nil {
		trace.TopicOnReaderError(r.cfg.Tracer, r.readConnectionID, err)
		_ = r.CloseWithError(r.ctx, err)
	}
	return err
}

func (r *topicStreamReaderImpl) startLoops() error {
	if err := r.setStarted(); err != nil {
		return err
	}

	r.backgroundWorkers.Start("readMessagesLoop", r.readMessagesLoop)
	r.backgroundWorkers.Start("dataRequestLoop", r.dataRequestLoop)
	r.backgroundWorkers.Start("updateTokenLoop", r.updateTokenLoop)

	r.backgroundWorkers.Start("consumeRawMessageFromBuffer", r.consumeRawMessageFromBuffer)

	return nil
}

func (r *topicStreamReaderImpl) setStarted() error {
	r.m.Lock()
	defer r.m.Unlock()

	if r.started {
		return xerrors.WithStackTrace(errors.New("already started"))
	}

	r.started = true
	return nil
}

func (r *topicStreamReaderImpl) initSession() (err error) {
	initMessage := r.cfg.initMessage()

	onDone := trace.TopicOnReaderInit(r.cfg.Tracer, r.readConnectionID, initMessage)
	defer func() {
		onDone(r.readConnectionID, err)
	}()

	if err = r.send(initMessage); err != nil {
		return err
	}

	resp, err := r.stream.Recv()
	if err != nil {
		return err
	}

	if status := resp.StatusData(); !status.Status.IsSuccess() {
		return xerrors.WithStackTrace(fmt.Errorf("bad status on initial error: %v (%v)", status.Status, status.Issues))
	}

	initResp, ok := resp.(*rawtopicreader.InitResponse)
	if !ok {
		return xerrors.WithStackTrace(fmt.Errorf("bad message type on session init: %v (%v)", resp, reflect.TypeOf(resp)))
	}

	r.readConnectionID = initResp.SessionID

	return nil
}

func (r *topicStreamReaderImpl) addRestBufferBytes(delta int) int {
	val := atomic.AddInt64(&r.atomicRestBufferSizeBytes, int64(delta))
	if val <= 0 {
		r.batcher.IgnoreMinRestrictionsOnNextPop()
	}
	return int(val)
}

func (r *topicStreamReaderImpl) getRestBufferBytes() int {
	return int(atomic.LoadInt64(&r.atomicRestBufferSizeBytes))
}

func (r *topicStreamReaderImpl) readMessagesLoop(ctx context.Context) {
	ctx, cancel := xcontext.WithErrCancel(ctx)
	defer cancel(xerrors.NewWithIssues("ydb: topic stream reader messages loop finished"))

	for {
		serverMessage, err := r.stream.Recv()
		if err != nil {
			trace.TopicOnReaderError(r.cfg.Tracer, r.readConnectionID, err)
			if errors.Is(err, rawtopicreader.ErrUnexpectedMessageType) {
				trace.TopicOnReaderUnknownGrpcMessage(r.cfg.Tracer, r.readConnectionID, err)
				// new messages can be added to protocol, it must be backward compatible to old programs
				// and skip message is safe
				continue
			}
			_ = r.CloseWithError(ctx, err)
			return
		}

		status := serverMessage.StatusData()
		if !status.Status.IsSuccess() {
			_ = r.CloseWithError(ctx,
				xerrors.WithStackTrace(
					fmt.Errorf("ydb: bad status from pq grpc stream: %v, %v", status.Status, status.Issues.String()),
				),
			)
		}

		switch m := serverMessage.(type) {
		case *rawtopicreader.ReadResponse:
			if err = r.onReadResponse(m); err != nil {
				_ = r.CloseWithError(ctx, err)
			}
		case *rawtopicreader.StartPartitionSessionRequest:
			if err = r.onStartPartitionSessionRequest(m); err != nil {
				_ = r.CloseWithError(ctx, err)
				return
			}
		case *rawtopicreader.StopPartitionSessionRequest:
			if err = r.onStopPartitionSessionRequest(m); err != nil {
				_ = r.CloseWithError(ctx, err)
				return
			}
		case *rawtopicreader.CommitOffsetResponse:
			if err = r.onCommitResponse(m); err != nil {
				_ = r.CloseWithError(ctx, err)
				return
			}

		case *rawtopicreader.UpdateTokenResponse:
			// skip
		default:
			trace.TopicOnReaderUnknownGrpcMessage(
				r.cfg.Tracer,
				r.readConnectionID,
				xerrors.WithStackTrace(xerrors.Wrap(fmt.Errorf(
					"ydb: unexpected message type in stream reader: %v",
					reflect.TypeOf(serverMessage),
				))),
			)
		}
	}
}

func (r *topicStreamReaderImpl) dataRequestLoop(ctx context.Context) {
	if r.ctx.Err() != nil {
		return
	}

	doneChan := ctx.Done()

	for {
		select {
		case <-doneChan:
			_ = r.CloseWithError(ctx, r.ctx.Err())
			return

		case free := <-r.freeBytes:
			sum := free

			// consume all messages from order and compress it to one data request
		forConsumeRequests:
			for {
				select {
				case free = <-r.freeBytes:
					sum += free
				default:
					break forConsumeRequests
				}
			}

			resCapacity := r.addRestBufferBytes(sum)
			trace.TopicOnReaderSentDataRequest(r.cfg.Tracer, r.readConnectionID, sum, resCapacity)
			if err := r.sendDataRequest(sum); err != nil {
				return
			}
		}
	}
}

func (r *topicStreamReaderImpl) sendDataRequest(size int) error {
	return r.send(&rawtopicreader.ReadRequest{BytesSize: size})
}

func (r *topicStreamReaderImpl) freeBufferFromMessages(batch *PublicBatch) {
	size := 0
	for messageIndex := range batch.Messages {
		size += batch.Messages[messageIndex].bufferBytesAccount
	}
	select {
	case r.freeBytes <- size:
	case <-r.ctx.Done():
	}
}

func (r *topicStreamReaderImpl) updateTokenLoop(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.CredUpdateInterval)
	defer ticker.Stop()

	readerCancel := ctx.Done()
	for {
		select {
		case <-readerCancel:
			return
		case <-ticker.C:
			r.updateToken(r.ctx)
		}
	}
}

func (r *topicStreamReaderImpl) onReadResponse(msg *rawtopicreader.ReadResponse) (err error) {
	resCapacity := r.addRestBufferBytes(-msg.BytesSize)
	onDone := trace.TopicOnReaderReceiveDataResponse(r.cfg.Tracer, r.readConnectionID, resCapacity, msg)
	defer func() {
		onDone(err)
	}()

	batchesCount := 0
	for i := range msg.PartitionData {
		batchesCount += len(msg.PartitionData[i].Batches)
	}

	var batches []*PublicBatch
	for pIndex := range msg.PartitionData {
		p := &msg.PartitionData[pIndex]

		// normal way
		session, err := r.sessionController.Get(p.PartitionSessionID)
		if err != nil {
			return err
		}

		for bIndex := range p.Batches {
			if r.ctx.Err() != nil {
				return r.ctx.Err()
			}

			batch, err := newBatchFromStream(r.cfg.Decoders, session, p.Batches[bIndex])
			if err != nil {
				return err
			}
			batches = append(batches, batch)
		}
	}

	if err := splitBytesByMessagesInBatches(batches, msg.BytesSize); err != nil {
		return err
	}

	for i := range batches {
		if err := r.batcher.PushBatches(batches[i]); err != nil {
			return err
		}
	}

	return nil
}

func (r *topicStreamReaderImpl) CloseWithError(ctx context.Context, reason error) (closeErr error) {
	onDone := trace.TopicOnReaderClose(r.cfg.Tracer, r.readConnectionID, reason)
	defer onDone(closeErr)

	isFirstClose := false
	r.m.WithLock(func() {
		if r.closed {
			return
		}
		isFirstClose = true
		r.closed = true

		r.err = reason
		r.cancel(reason)
	})
	if !isFirstClose {
		return nil
	}

	closeErr = r.committer.Close(ctx, reason)

	batcherErr := r.batcher.Close(reason)
	if closeErr == nil {
		closeErr = batcherErr
	}

	// close stream strong after committer close - for flush commits buffer
	streamCloseErr := r.stream.CloseSend()
	if closeErr == nil {
		closeErr = streamCloseErr
	}

	// close background workers after r.stream.CloseSend
	bgCloseErr := r.backgroundWorkers.Close(ctx, reason)
	if closeErr == nil {
		closeErr = bgCloseErr
	}
	return closeErr
}

func (r *topicStreamReaderImpl) onCommitResponse(msg *rawtopicreader.CommitOffsetResponse) error {
	for i := range msg.PartitionsCommittedOffsets {
		commit := &msg.PartitionsCommittedOffsets[i]
		partition, err := r.sessionController.Get(commit.PartitionSessionID)
		if err != nil {
			return fmt.Errorf("ydb: can't found session on commit response: %w", err)
		}
		partition.setCommittedOffset(commit.CommittedOffset)

		trace.TopicOnReaderCommittedNotify(
			r.cfg.Tracer,
			r.readConnectionID,
			partition.Topic,
			partition.PartitionID,
			partition.partitionSessionID.ToInt64(),
			commit.CommittedOffset.ToInt64(),
		)

		r.committer.OnCommitNotify(partition, commit.CommittedOffset)
	}

	return nil
}

func (r *topicStreamReaderImpl) updateToken(ctx context.Context) {
	onUpdateToken := trace.TopicOnReaderUpdateToken(
		r.cfg.Tracer,
		r.readConnectionID,
	)
	token, err := r.cfg.Cred.Token(ctx)
	onSent := onUpdateToken(len(token), err)
	if err != nil {
		return
	}

	err = r.send(&rawtopicreader.UpdateTokenRequest{UpdateTokenRequest: rawtopiccommon.UpdateTokenRequest{Token: token}})
	onSent(err)
}

func (r *topicStreamReaderImpl) onStartPartitionSessionRequest(m *rawtopicreader.StartPartitionSessionRequest) error {
	session := newPartitionSession(
		r.ctx,
		m.PartitionSession.Path,
		m.PartitionSession.PartitionID,
		m.PartitionSession.PartitionSessionID,
		m.CommittedOffset,
	)
	if err := r.sessionController.Add(session); err != nil {
		return err
	}
	return r.batcher.PushRawMessage(session, m)
}

func (r *topicStreamReaderImpl) onStartPartitionSessionRequestFromBuffer(
	m *rawtopicreader.StartPartitionSessionRequest,
) (err error) {
	session, err := r.sessionController.Get(m.PartitionSession.PartitionSessionID)
	if err != nil {
		return err
	}

	onDone := trace.TopicOnReaderPartitionReadStartResponse(
		r.cfg.Tracer,
		r.readConnectionID,
		session.Context(),
		session.Topic,
		session.PartitionID,
		session.partitionSessionID.ToInt64(),
	)

	respMessage := &rawtopicreader.StartPartitionSessionResponse{
		PartitionSessionID: session.partitionSessionID,
	}

	var forceOffset *int64
	var commitOffset *int64

	defer func() {
		onDone(forceOffset, commitOffset, err)
	}()

	if r.cfg.GetPartitionStartOffsetCallback != nil {
		req := PublicGetPartitionStartOffsetRequest{
			Topic:       session.Topic,
			PartitionID: session.PartitionID,
		}
		resp, callbackErr := r.cfg.GetPartitionStartOffsetCallback(session.Context(), req)
		if callbackErr != nil {
			return callbackErr
		}
		if resp.startOffsetUsed {
			wantOffset := resp.startOffset.ToInt64()
			forceOffset = &wantOffset
		}
	}

	respMessage.ReadOffset.FromInt64Pointer(forceOffset)
	if r.cfg.CommitMode.commitsEnabled() {
		commitOffset = forceOffset
		respMessage.CommitOffset.FromInt64Pointer(commitOffset)
	}

	return r.send(respMessage)
}

func (r *topicStreamReaderImpl) onStopPartitionSessionRequest(m *rawtopicreader.StopPartitionSessionRequest) error {
	session, err := r.sessionController.Get(m.PartitionSessionID)
	if err != nil {
		return err
	}

	if !m.Graceful {
		session.Close(xerrors.WithStackTrace(errPartitionSessionStoppedByServer))
	}

	return r.batcher.PushRawMessage(session, m)
}
