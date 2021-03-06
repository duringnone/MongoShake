package collector

import (
	"fmt"
	"time"

	"mongoshake/collector/ckpt"
	"mongoshake/collector/configure"
	"mongoshake/collector/filter"
	"mongoshake/common"
	"mongoshake/oplog"
	"mongoshake/quorum"
	"mongoshake/collector/reader"

	"github.com/gugemichael/nimo4go"
	LOG "github.com/vinllen/log4go"
	"github.com/vinllen/mgo/bson"
	"strings"
)

const (
	// FetcherBufferCapacity   = 256
	// AdaptiveBatchingMaxSize = 16384 // 16k

	// bson deserialize workload is CPU-intensive task
	PipelineQueueMaxNr = 4
	PipelineQueueMinNr = 1
	PipelineQueueLen   = 64

	DurationTime                  = 6000 // unit: ms.
	DDLCheckpointInterval         = 300  // unit: ms.
	FilterCheckpointGap           = 180  // unit: seconds. no checkpoint update, flush checkpoint mandatory
	FilterCheckpointCheckInterval = 180  // unit: seconds.
	CheckCheckpointUpdateTimes    = 10   // at most times of time check
)

type OplogHandler interface {
	// invocation on every oplog consumed
	Handle(log *oplog.PartialLog)
}

// OplogSyncer poll oplogs from original source MongoDB.
type OplogSyncer struct {
	OplogHandler

	// source mongodb replica set name
	replset string
	// oplog start position of source mongodb
	startPosition int64
	// full sync finish position, used to check DDL between full sync and incr sync
	fullSyncFinishPosition bson.MongoTimestamp
	// pass from coordinator
	rateController *nimo.SimpleRateController

	ckptManager *ckpt.CheckpointManager

	// oplog hash strategy
	hasher oplog.Hasher

	// pending queue. used by raw log parsing. we buffered the
	// target raw oplogs in buffer and push them to pending queue
	// when buffer is filled in. and transfer to log queue
	// buffer            []*bson.Raw // move to persister
	PendingQueue []chan [][]byte
	logsQueue    []chan []*oplog.GenericOplog
	// nextQueuePosition uint64 // move to persister

	// source mongo oplog/event reader
	reader sourceReader.Reader
	// journal log that records all oplogs
	journal *utils.Journal
	// oplogs dispatcher
	batcher *Batcher
	// data persist handler
	persister *Persister

	// qos
	qos *utils.Qos

	// timers for inner event
	startTime time.Time
	ckptTime  time.Time

	replMetric *utils.ReplicationMetric
}

/*
 * Syncer is used to fetch oplog from source MongoDB and then send to different workers which can be seen as
 * a network sender. There are several syncer coexist to improve the fetching performance.
 * The data flow in syncer is:
 * source mongodb --> reader --> persister --> pending queue(raw data) --> logs queue(parsed data) --> worker
 * The reason we split pending queue and logs queue is to improve the performance.
 */
func NewOplogSyncer(
	replset string,
	startPosition int64,
	fullSyncFinishPosition int64,
	mongoUrl string,
	gids []string,
	rateController *nimo.SimpleRateController) *OplogSyncer {

	reader, err := sourceReader.CreateReader(conf.Options.IncrSyncMongoFetchMethod, mongoUrl, replset)
	if err != nil {
		LOG.Critical("create reader with url[%v] replset[%v] failed[%v]", mongoUrl, replset, err)
		return nil
	}

	syncer := &OplogSyncer{
		replset:                replset,
		startPosition:          startPosition,
		fullSyncFinishPosition: bson.MongoTimestamp(fullSyncFinishPosition),
		rateController:         rateController,
		journal: utils.NewJournal(utils.JournalFileName(
			fmt.Sprintf("%s.%s", conf.Options.Id, replset))),
		reader: reader,
		qos:    utils.StartQoS(0, 1, &utils.IncrSentinelOptions.TPS), // default is 0 which means do not limit
	}

	// concurrent level hasher
	switch conf.Options.IncrSyncShardKey {
	case oplog.ShardByNamespace:
		syncer.hasher = &oplog.TableHasher{}
	case oplog.ShardByID:
		syncer.hasher = &oplog.PrimaryKeyHasher{}
	}

	filterList := filter.OplogFilterChain{new(filter.AutologousFilter), new(filter.NoopFilter), filter.NewGidFilter(gids)}

	// DDL filter
	if !conf.Options.FilterDDLEnable {
		filterList = append(filterList, new(filter.DDLFilter))
	}
	// namespace filter, heavy operation
	if len(conf.Options.FilterNamespaceWhite) != 0 || len(conf.Options.FilterNamespaceBlack) != 0 {
		namespaceFilter := filter.NewNamespaceFilter(conf.Options.FilterNamespaceWhite,
			conf.Options.FilterNamespaceBlack)
		filterList = append(filterList, namespaceFilter)
	}

	// oplog filters. drop the oplog if any of the filter
	// list returns true. The order of all filters is not significant.
	// workerGroup is assigned later by syncer.bind()
	syncer.batcher = NewBatcher(syncer, filterList, syncer, []*Worker{})

	// init persist
	syncer.persister = NewPersister(replset, syncer)

	return syncer
}

func (sync *OplogSyncer) Init() {
	var options uint64 = utils.METRIC_CKPT_TIMES| utils.METRIC_LSN| utils.METRIC_SUCCESS|
		utils.METRIC_TPS | utils.METRIC_FILTER
	if conf.Options.Tunnel != utils.VarTunnelDirect {
		options |= utils.METRIC_RETRANSIMISSION
		options |= utils.METRIC_TUNNEL_TRAFFIC
		options |= utils.METRIC_WORKER
	}

	sync.replMetric = utils.NewMetric(sync.replset, utils.TypeIncr, options)
	sync.replMetric.ReplStatus.Update(utils.WorkGood)

	sync.RestAPI()
	sync.persister.RestAPI()
}

// bind different worker
func (sync *OplogSyncer) Bind(w *Worker) {
	sync.batcher.workerGroup = append(sync.batcher.workerGroup, w)
}

func (sync *OplogSyncer) StartDiskApply() {
	sync.persister.SetFetchStage(utils.FetchStageStoreDiskApply)
}

// start to polling oplog
func (sync *OplogSyncer) Start() {
	LOG.Info("Poll oplog syncer start. ckpt_interval[%dms], gid[%s], shard_key[%s]",
		conf.Options.CheckpointInterval, conf.Options.IncrSyncOplogGIDS, conf.Options.IncrSyncShardKey)

	sync.startTime = time.Now()

	// start persister
	sync.persister.Start()

	// process about the checkpoint :
	//
	// 1. create checkpoint manager
	// 2. load existing ckpt from remote storage
	// 3. start checkpoint persist routine
	sync.newCheckpointManager(sync.replset, sync.startPosition)
	// load checkpoint and set stage
	if err := sync.loadCheckpoint(); err != nil {
		LOG.Crash(err)
	}

	// start deserializer: parse data from pending queue, and then push into logs queue.
	sync.startDeserializer()
	// start batcher: pull oplog from logs queue and then batch together before adding into worker.
	sync.startBatcher()

	// forever fetching oplog from mongodb into oplog_reader
	for {
		sync.poll()

		// error or exception occur
		LOG.Warn("Oplog syncer polling yield. master:%t, yield:%dms", quorum.IsMaster(), DurationTime)
		utils.YieldInMs(DurationTime)
	}
}

// fetch all oplog from logs queue, batched together and then send to different workers.
func (sync *OplogSyncer) startBatcher() {
	var batcher = sync.batcher
	filterCheckTs := time.Now()
	filterFlag := false // marks whether previous log is filter

	nimo.GoRoutineInLoop(func() {
		// As much as we can batch more from logs queue. batcher can merge
		// a sort of oplogs from different logs queue one by one. the max number
		// of oplogs in batch is limited by AdaptiveBatchingMaxSize
		batchedOplog, barrier, allEmpty := batcher.BatchMore()

		var newestTs bson.MongoTimestamp
		if log, filterLog := batcher.getLastOplog(); log != nil && !allEmpty {
			// if all filtered, still update checkpoint
			newestTs = log.Timestamp

			// push to worker
			if worked := batcher.dispatchBatches(batchedOplog); worked {
				sync.replMetric.SetLSN(utils.TimestampToInt64(newestTs))
				// update latest fetched timestamp in memory
				sync.reader.UpdateQueryTimestamp(newestTs)
			}

			filterFlag = false

			// flush checkpoint value
			sync.checkpoint(barrier, 0)
			sync.checkCheckpointUpdate(barrier, newestTs) // check if need
		} else {
			// if log is nil, check whether filterLog is empty
			if filterLog == nil {
				return
			} else {
				now := time.Now()

				// return if filterFlag == false
				if filterFlag == false {
					filterFlag = true
					filterCheckTs = now
					return
				}

				// pass only if all received oplog are filtered for {FilterCheckpointCheckInterval} seconds.
				if now.After(filterCheckTs.Add(FilterCheckpointCheckInterval*time.Second)) == false {
					return
				}

				checkpointTs := utils.ExtractMongoTimestamp(sync.ckptManager.GetInMemory().Timestamp)
				filterNewestTs := utils.ExtractMongoTimestamp(filterLog.Timestamp)
				if filterNewestTs-FilterCheckpointGap > checkpointTs {
					// if checkpoint has not been update for {FilterCheckpointGap} seconds, update
					// checkpoint mandatory.
					newestTs = filterLog.Timestamp
					LOG.Info("try to update checkpoint mandatory from %v to %v",
						utils.ExtractTimestampForLog(sync.ckptManager.GetInMemory().Timestamp),
						utils.ExtractTimestampForLog(filterLog.Timestamp))
				} else {
					return
				}
			}

			filterFlag = false

			if log != nil {
				newestTsLog := utils.ExtractTimestampForLog(newestTs)
				if newestTs < log.Timestamp {
					LOG.Crashf("filter newestTs[%v] smaller than previous timestamp[%v]",
						newestTsLog, utils.ExtractTimestampForLog(log.Timestamp))
				}

				LOG.Info("waiting last checkpoint[%v] updated", newestTsLog)
				// check last checkpoint updated

				status := sync.checkCheckpointUpdate(true, log.Timestamp)
				LOG.Info("last checkpoint[%v] updated [%v]", newestTsLog, status)
			} else {
				LOG.Info("last log is empty, skip waiting checkpoint updated")
			}

			// update latest fetched timestamp in memory
			sync.reader.UpdateQueryTimestamp(newestTs)
			// flush checkpoint by the newest filter oplog value
			sync.checkpoint(false, newestTs)
			return
		}
	})
}

func (sync *OplogSyncer) checkCheckpointUpdate(barrier bool, newestTs bson.MongoTimestamp) bool {
	// if barrier == true, we should check whether the checkpoint is updated to `newestTs`.
	if barrier && newestTs > 0 && conf.Options.IncrSyncWorker > 1 {
		LOG.Info("find barrier")
		var checkpointTs bson.MongoTimestamp
		for i := 0; i < CheckCheckpointUpdateTimes; i++ {
			// checkpointTs := sync.ckptManager.GetInMemory().Timestamp
			checkpoint, _, err := sync.ckptManager.Get()
			if err != nil {
				LOG.Error("get remote checkpoint failed: %v", err)
				utils.YieldInMs(DDLCheckpointInterval * 3)
				continue
			}

			checkpointTs = checkpoint.Timestamp

			LOG.Info("compare remote checkpoint[%v] to local newestTs[%v]",
				utils.ExtractTimestampForLog(checkpointTs), utils.ExtractTimestampForLog(newestTs))
			if checkpointTs >= newestTs {
				LOG.Info("barrier checkpoint updated to newest[%v]", utils.ExtractTimestampForLog(newestTs))
				return true
			}
			utils.YieldInMs(DDLCheckpointInterval)

			// re-flush
			sync.checkpoint(true, 0)
		}

		/*
		 * if code hits here, it means the checkpoint has not been updated(usually DDL).
		 * it's ok because the checkpoint can still forward on the next time.
		 * However, if MongoShake crashes here and restarts, there maybe a conflict when the
		 * oplog is DDL that has been applied but checkpoint not updated.
		 */
		LOG.Warn("check checkpoint[%v] update[%v] failed, but do worry",
			utils.ExtractTimestampForLog(checkpointTs), utils.ExtractTimestampForLog(newestTs))
	}
	return false
}

/********************************deserializer begin**********************************/
// deserializer: pending_queue -> logs_queue

// how many pending queue we create
func calculatePendingQueueConcurrency() int {
	// single {pending|logs}queue while it'is multi source shard
	if conf.Options.IsShardCluster() {
		return PipelineQueueMinNr
	}
	return PipelineQueueMaxNr
}

// deserializer: fetch oplog from pending queue, parsed and then add into logs queue.
func (sync *OplogSyncer) startDeserializer() {
	parallel := calculatePendingQueueConcurrency()
	sync.PendingQueue = make([]chan [][]byte, parallel, parallel)
	sync.logsQueue = make([]chan []*oplog.GenericOplog, parallel, parallel)
	for index := 0; index != len(sync.PendingQueue); index++ {
		sync.PendingQueue[index] = make(chan [][]byte, PipelineQueueLen)
		sync.logsQueue[index] = make(chan []*oplog.GenericOplog, PipelineQueueLen)
		go sync.deserializer(index)
	}
}

func (sync *OplogSyncer) deserializer(index int) {
	// parser is used to parse the raw []byte
	var parser func(input []byte) (*oplog.PartialLog, error)
	if conf.Options.IncrSyncMongoFetchMethod == utils.VarIncrSyncMongoFetchMethodChangeStream {
		// parse []byte (change stream event format) -> oplog
		parser = func(input []byte) (*oplog.PartialLog, error) {
			return oplog.ConvertEvent2Oplog(input)
		}
	} else {
		// parse []byte (oplog format) -> oplog
		parser = func(input []byte) (*oplog.PartialLog, error) {
			log := oplog.ParsedLog{}
			err := bson.Unmarshal(input, &log)
			return &oplog.PartialLog{
				ParsedLog: log,
			}, err
		}
	}

	// combiner is used to combine data and send to downstream
	var combiner func(raw []byte, log *oplog.PartialLog) *oplog.GenericOplog
	// change stream && !direct && !(kafka & json)
	if conf.Options.IncrSyncMongoFetchMethod == utils.VarIncrSyncMongoFetchMethodChangeStream &&
		conf.Options.Tunnel != utils.VarTunnelDirect &&
		!(conf.Options.Tunnel == utils.VarTunnelKafka &&
			conf.Options.TunnelMessage == utils.VarTunnelMessageJson) {
		// very time consuming!
		combiner = func(raw []byte, log *oplog.PartialLog) *oplog.GenericOplog {
			if out, err := bson.Marshal(log.ParsedLog); err != nil {
				LOG.Crashf("deserializer marshal[%v] failed: %v", log.ParsedLog, err)
				return nil
			} else {
				return &oplog.GenericOplog{Raw: out, Parsed: log}
			}
		}
	} else {
		combiner = func(raw []byte, log *oplog.PartialLog) *oplog.GenericOplog {
			return &oplog.GenericOplog{Raw: raw, Parsed: log}
		}
	}

	// run
	for {
		batchRawLogs := <-sync.PendingQueue[index]
		nimo.AssertTrue(len(batchRawLogs) != 0, "pending queue batch logs has zero length")
		var deserializeLogs = make([]*oplog.GenericOplog, 0, len(batchRawLogs))

		for _, rawLog := range batchRawLogs {
			log, err := parser(rawLog)
			if err != nil {
				LOG.Crashf("deserializer parse data failed[%v]", err)
			}
			log.RawSize = len(rawLog)
			deserializeLogs = append(deserializeLogs, combiner(rawLog, log))
		}
		sync.logsQueue[index] <- deserializeLogs
	}
}

/********************************deserializer end**********************************/

// only master(maybe several mongo-shake start) can poll oplog.
func (sync *OplogSyncer) poll() {
	// we should reload checkpoint. in case of other collector
	// has fetched oplogs when master quorum leader election
	// happens frequently. so we simply reload.
	checkpoint, _, err := sync.ckptManager.Get()
	if err != nil {
		// we doesn't continue working on ckpt fetched failed. because we should
		// confirm the exist checkpoint value or exactly knows that it doesn't exist
		LOG.Critical("Acquire the existing checkpoint from remote[%s %s.%s] failed !",
			conf.Options.CheckpointStorage, conf.Options.CheckpointStorageDb,
			conf.Options.CheckpointStorageCollection)
		return
	}
	sync.reader.SetQueryTimestampOnEmpty(checkpoint.Timestamp)
	sync.reader.StartFetcher() // start reader fetcher if not exist

	for quorum.IsMaster() {
		// limit the qps if enabled
		if sync.qos.Limit > 0 {
			sync.qos.FetchBucket()
		}

		// only get one
		sync.next()
	}
}

// fetch oplog from reader.
func (sync *OplogSyncer) next() bool {
	var log []byte
	var err error
	if log, err = sync.reader.Next(); log != nil {
		payload := int64(len(log))
		sync.replMetric.AddGet(1)
		sync.replMetric.SetOplogMax(payload)
		sync.replMetric.SetOplogAvg(payload)
		sync.replMetric.ReplStatus.Clear(utils.FetchBad)
	} else if err == sourceReader.CollectionCappedError {
		LOG.Error("oplog collection capped error, users should fix it manually")
		utils.YieldInMs(DurationTime)
		return false
	} else if err != nil && err != sourceReader.TimeoutError {
		LOG.Error("syncer[%s] internal error: %v", sync.reader.Name(), err)
		// error is nil indicate that only timeout incur syncer.next()
		// return false. so we regardless that
		if sync.isCrashError(err.Error()) {
			LOG.Crashf("I can't handle this error, please solve it manually!")
		}

		sync.replMetric.ReplStatus.Update(utils.FetchBad)
		utils.YieldInMs(DurationTime)

		// alarm
	}

	// buffered oplog or trigger to flush. log is nil
	// means that we need to flush buffer right now

	// inject into persist handler
	sync.persister.Inject(log)
	return true
}

func (sync *OplogSyncer) isCrashError(errMsg string) bool {
	if conf.Options.IncrSyncMongoFetchMethod == utils.VarIncrSyncMongoFetchMethodChangeStream &&
		strings.Contains(errMsg, sourceReader.ErrInvalidStartPosition) {
		return true
	}
	return false
}

func (sync *OplogSyncer) Handle(log *oplog.PartialLog) {
	// 1. records audit log if need
	sync.journal.WriteRecord(log)
}

func (sync *OplogSyncer) RestAPI() {
	type Time struct {
		TimestampUnix int64  `json:"unix"`
		TimestampTime string `json:"time"`
	}
	type MongoTime struct {
		Time
		TimestampMongo string `json:"ts"`
	}

	type Info struct {
		Who         string     `json:"who"`
		Tag         string     `json:"tag"`
		ReplicaSet  string     `json:"replset"`
		Logs        uint64     `json:"logs_get"`
		LogsRepl    uint64     `json:"logs_repl"`
		LogsSuccess uint64     `json:"logs_success"`
		Tps         uint64     `json:"tps"`
		Lsn         *MongoTime `json:"lsn"`
		LsnAck      *MongoTime `json:"lsn_ack"`
		LsnCkpt     *MongoTime `json:"lsn_ckpt"`
		Now         *Time      `json:"now"`
	}

	// total replication info
	utils.IncrSyncHttpApi.RegisterAPI("/repl", nimo.HttpGet, func([]byte) interface{} {
		return &Info{
			Who:         conf.Options.Id,
			Tag:         utils.BRANCH,
			ReplicaSet:  sync.replset,
			Logs:        sync.replMetric.Get(),
			LogsRepl:    sync.replMetric.Apply(),
			LogsSuccess: sync.replMetric.Success(),
			Tps:         sync.replMetric.Tps(),
			Lsn: &MongoTime{TimestampMongo: utils.Int64ToString(sync.replMetric.LSN),
				Time: Time{TimestampUnix: utils.ExtractMongoTimestamp(sync.replMetric.LSN),
					TimestampTime: utils.TimestampToString(utils.ExtractMongoTimestamp(sync.replMetric.LSN))}},
			LsnCkpt: &MongoTime{TimestampMongo: utils.Int64ToString(sync.replMetric.LSNCheckpoint),
				Time: Time{TimestampUnix: utils.ExtractMongoTimestamp(sync.replMetric.LSNCheckpoint),
					TimestampTime: utils.TimestampToString(utils.ExtractMongoTimestamp(sync.replMetric.LSNCheckpoint))}},
			LsnAck: &MongoTime{TimestampMongo: utils.Int64ToString(sync.replMetric.LSNAck),
				Time: Time{TimestampUnix: utils.ExtractMongoTimestamp(sync.replMetric.LSNAck),
					TimestampTime: utils.TimestampToString(utils.ExtractMongoTimestamp(sync.replMetric.LSNAck))}},
			Now: &Time{TimestampUnix: time.Now().Unix(), TimestampTime: utils.TimestampToString(time.Now().Unix())},
		}
	})

	// queue size info
	type InnerQueue struct {
		Id           uint   `json:"queue_id"`
		PendingQueue uint64 `json:"pending_queue_used"`
		LogsQueue    uint64 `json:"logs_queue_used"`
	}
	type Queue struct {
		SyncerId            string       `json:"syncer_replica_set_name"`
		LogsQueuePerSize    int          `json:"logs_queue_size"`
		PendingQueuePerSize int          `json:"pending_queue_size"`
		InnerQueue          []InnerQueue `json:"syncer_inner_queue"`
		PersisterBufferUsed int          `json:"persister_buffer_used"`
	}

	utils.IncrSyncHttpApi.RegisterAPI("/queue", nimo.HttpGet, func([]byte) interface{} {
		queue := make([]InnerQueue, calculatePendingQueueConcurrency())
		for i := 0; i < len(queue); i++ {
			queue[i] = InnerQueue{
				Id:           uint(i),
				PendingQueue: uint64(len(sync.PendingQueue[i])),
				LogsQueue:    uint64(len(sync.logsQueue[i])),
			}
		}
		return &Queue{
			SyncerId:            sync.replset,
			LogsQueuePerSize:    cap(sync.logsQueue[0]),
			PendingQueuePerSize: cap(sync.PendingQueue[0]),
			InnerQueue:          queue,
			PersisterBufferUsed: len(sync.persister.Buffer),
		}
	})
}
