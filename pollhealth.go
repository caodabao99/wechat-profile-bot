package main

import (
	"sync/atomic"
	"time"
)

// 微信长轮询健康状态。由 main 的轮询循环写入、/api/status 读取。
// 用原子变量而非互斥锁：读取发生在 HTTP 请求里，不能被轮询循环阻塞。
var (
	pollLastOK     atomic.Int64 // 最后一次 GetUpdates 无错误的时刻（unixnano），0=从未成功
	pollLastErrAt  atomic.Int64 // 最近一次轮询错误时刻（unixnano），0=无错误
	pollLastErrMsg atomic.Value // 最近一次轮询错误文案（string）
)

// PollStatus 供 /api/status 序列化
type PollStatus struct {
	LastOKSecAgo  int64  `json:"lastOkSecAgo"`  // 距上次成功轮询的秒数，-1=从未成功
	LastErrSecAgo int64  `json:"lastErrSecAgo"` // 距上次轮询报错的秒数，-1=无错误
	LastErrMsg    string `json:"lastErrMsg"`
}

func recordPollOK() {
	pollLastOK.Store(time.Now().UnixNano())
}

func recordPollErr(err error) {
	if err == nil {
		return
	}
	pollLastErrAt.Store(time.Now().UnixNano())
	pollLastErrMsg.Store(err.Error())
}

func snapshotPollStatus() PollStatus {
	now := time.Now().UnixNano()
	out := PollStatus{LastOKSecAgo: -1, LastErrSecAgo: -1}
	if v := pollLastOK.Load(); v > 0 {
		out.LastOKSecAgo = (now - v) / int64(time.Second)
	}
	if v := pollLastErrAt.Load(); v > 0 {
		out.LastErrSecAgo = (now - v) / int64(time.Second)
	}
	if msg, ok := pollLastErrMsg.Load().(string); ok {
		out.LastErrMsg = msg
	}
	return out
}
