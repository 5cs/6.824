package raftkv

import (
	// "helper" // Applier interface
	"bytes"
	"fmt"
	"labgob"
	"labrpc"
	"log"
	"raft"
	"sync"
	// "time"
)

const Debug = 0

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug > 0 {
		log.Printf(format, a...)
	}
	return
}

type Op struct {
	// Your definitions here.
	// Field names must start with capital letters,
	// otherwise RPC will break.
	Op    string
	Key   string
	Value string
}

type KVServer struct {
	mu      sync.Mutex
	me      int
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg

	maxraftstate int // snapshot if log grows this big

	// Your definitions here.
	persister         *raft.Persister
	db                map[string]string
	clientSeqs        map[int64]int64
	notices           map[int]*sync.Cond
	appliedCmds       map[int]*appliedResult
	lastIncludedIndex int
	lastIncludedTerm  int
	restart           bool
}

type appliedResult struct {
	Key    string
	Result interface{}
}

func getFmtKey(args *GetArgs) string {
	return fmt.Sprintf("%v_%v_%v", args.Key, args.ClientId, args.Seq)
}

func putAppendFmtKey(args *PutAppendArgs) string {
	return fmt.Sprintf("%v_%v_%v", args.Key, args.ClientId, args.Seq)
}

func (kv *KVServer) Get(args *GetArgs, reply *GetReply) {
	// Your code here.
	if _, isLeader := kv.rf.GetState(); !isLeader {
		reply.WrongLeader = true
		return
	}

	cmd := *args
	for {
		*reply = GetReply{}
		index, _, isLeader := kv.rf.Start(cmd)
		if !isLeader {
			reply.WrongLeader = true
			return
		}

		kv.mu.Lock()
		if _, ok := kv.notices[index]; !ok {
			kv.notices[index] = sync.NewCond(&kv.mu)
		}
		kv.notices[index].Wait()

		ret := kv.appliedCmds[index]
		k := getFmtKey(&cmd)
		if ret.Key == k {
			switch ret.Result.(type) {
			case GetReply:
				*reply = ret.Result.(GetReply)
				kv.mu.Unlock()
				return
			default:
			}
		}
		kv.mu.Unlock()
	}
}

func (kv *KVServer) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	// Your code here.
	if _, isLeader := kv.rf.GetState(); !isLeader {
		reply.WrongLeader = true
		return
	}
	kv.mu.Lock()
	if seq, ok := kv.clientSeqs[args.ClientId]; ok && args.Seq <= seq {
		reply.Err = OK
		kv.mu.Unlock()
		return // applied
	}
	kv.mu.Unlock()
	cmd := *args
	for {
		*reply = PutAppendReply{}
		index, _, isLeader := kv.rf.Start(cmd)
		if !isLeader {
			reply.WrongLeader = true
			return
		}

		kv.mu.Lock()
		if _, ok := kv.notices[index]; !ok {
			kv.notices[index] = sync.NewCond(&kv.mu)
		}
		kv.notices[index].Wait()

		ret := kv.appliedCmds[index]
		k := putAppendFmtKey(&cmd)
		if ret.Key == k {
			switch ret.Result.(type) {
			case PutAppendReply:
				*reply = ret.Result.(PutAppendReply)
				kv.mu.Unlock()
				return
			default:
			}
		}
		kv.mu.Unlock()
	}
}

//
// the tester calls Kill() when a KVServer instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
//
func (kv *KVServer) Kill() {
	kv.rf.Kill()
	// Your code here, if desired.
}

func (kv *KVServer) Name() string {
	return fmt.Sprintf("%#v", kv.me)
}

// Apply method for Raft
func (kv *KVServer) Apply(applyMsg interface{}) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	msg := applyMsg.(*raft.ApplyMsg)
	index := msg.CommandIndex
	cmd := msg.Command
	isLeader := msg.IsLeader
	var (
		key   string      = ""
		reply interface{} = nil
	)
	switch cmd.(type) {
	case GetArgs:
		args := cmd.(GetArgs)
		key = getFmtKey(&args)
		reply = kv.get(&args, index, isLeader)
	case PutAppendArgs:
		args := cmd.(PutAppendArgs)
		key = putAppendFmtKey(&args)
		reply = kv.putAppend(&args, index, isLeader)
	case *raft.InstallSnapshotArgs: // install snapshot
		args := cmd.(*raft.InstallSnapshotArgs)
		kv.readSnapshot(kv.persister.ReadSnapshot())
		DPrintf("Op \"InstallSnapshot\" at %#v, get values: %#v, reqId: %#v, leaderId-term-index:%#v-%#v-%#v\n",
			kv.me, kv.db, args.ReqId, args.LeaderId, args.LastIncludedTerm, args.LastIncludedIndex)
	default:
	}

	if reply != nil {
		kv.trySnapshot(index)
		// send result
		if _, ok := kv.notices[index]; ok {
			kv.appliedCmds[index] = &appliedResult{
				Key:    key,
				Result: reply,
			}
			kv.notices[index].Broadcast()
		}
	}
}

func (kv *KVServer) trySnapshot(index int) {
	if !(kv.maxraftstate != -1 &&
		float64(kv.persister.RaftStateSize()) >= float64(kv.maxraftstate)*0.8) {
		return
	}

	logs := kv.rf.GetLog()
	kv.lastIncludedIndex = index
	kv.lastIncludedTerm = logs[kv.rf.Index(index)].Term
	kv.persist()
	return
}

func (kv *KVServer) persist() {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	e.Encode(kv.lastIncludedIndex)
	e.Encode(kv.lastIncludedTerm)
	e.Encode(kv.clientSeqs)
	e.Encode(kv.db)
	data := w.Bytes()
	kv.persister.SaveSnapshot(data)
	kv.rf.TruncateLog(kv.lastIncludedIndex)
}

func (kv *KVServer) readSnapshot(data []byte) {
	if data == nil || len(data) < 1 {
		return
	}
	var (
		lastIncludedIndex int = 0
		lastIncludedTerm  int = 0
	)
	kv.db = make(map[string]string)
	kv.clientSeqs = make(map[int64]int64)
	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)
	d.Decode(&lastIncludedIndex)
	d.Decode(&lastIncludedTerm)
	d.Decode(&kv.clientSeqs)
	d.Decode(&kv.db)
	kv.lastIncludedIndex = lastIncludedIndex
	kv.lastIncludedTerm = lastIncludedTerm
}

func (kv *KVServer) putAppend(args *PutAppendArgs, index int, isLeader bool) PutAppendReply {
	if seq, ok := kv.clientSeqs[args.ClientId]; ok && seq >= args.Seq {
		return PutAppendReply{Err: OK}
	}
	kv.clientSeqs[args.ClientId] = args.Seq
	if args.Op == "Put" {
		kv.db[args.Key] = args.Value
	} else {
		kv.db[args.Key] += args.Value
	}
	return PutAppendReply{Err: OK}
}

func (kv *KVServer) get(args *GetArgs, index int, isLeader bool) GetReply {
	if value, ok := kv.db[args.Key]; !ok {
		return GetReply{Value: "", Err: ErrNoKey}
	} else {
		return GetReply{Value: value, Err: OK}
	}
}

//
// servers[] contains the ports of the set of
// servers that will cooperate via Raft to
// form the fault-tolerant key/value service.
// me is the index of the current server in servers[].
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
// the k/v server should snapshot when Raft's saved state exceeds maxraftstate bytes,
// in order to allow Raft to garbage-collect its log. if maxraftstate is -1,
// you don't need to snapshot.
// StartKVServer() must return quickly, so it should start goroutines
// for any long-running work.
//
func StartKVServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int) *KVServer {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})
	labgob.Register(GetArgs{})
	labgob.Register(PutAppendArgs{})
	labgob.Register(GetReply{})
	labgob.Register(PutAppendReply{})

	kv := new(KVServer)
	kv.me = me
	kv.maxraftstate = maxraftstate

	// You may need initialization code here.

	kv.applyCh = make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)

	// You may need initialization code here.
	kv.persister = persister
	kv.rf.SetApp(kv)
	kv.db = make(map[string]string)
	kv.clientSeqs = make(map[int64]int64)
	kv.notices = make(map[int]*sync.Cond)
	kv.appliedCmds = make(map[int]*appliedResult)
	kv.readSnapshot(kv.persister.ReadSnapshot())
	DPrintf("Op \"InstallSnapshot\" at %#v, get values: %#v, reqId: %#v\n", kv.me, kv.db, -1)

	// server loop
	go func() {
		for applyMsg := range kv.applyCh {
			if false {
				DPrintf("%#v", applyMsg)
			}
		}
	}()

	return kv
}
