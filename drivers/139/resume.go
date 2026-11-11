package _139

import (
	"fmt"
	"sync"
	"time"
)

// 139 云盘的分片上传是「会话式」的：
//   /file/create 拿到 fileId + uploadId（以及前 100 个分片的上传地址）
//   每个分片单独 PUT 到自己的 uploadUrl
//   /file/complete 收尾
//
// 网络抖动导致整次 Put 中断时，只要会话还在，下一次尝试就能跳过已成功的连续分片，
// 从断点继续 —— 不必把几十 GB 重新读一遍、重新加密一遍、重新传一遍。
//
// 会话只放在进程内存里：上传任务本身不持久化（Tasks.Upload.TaskPersistant=false），
// 进程重启后任务也没了，所以没必要落盘。默认保留 24 小时。
const uploadSessionTTL = 24 * time.Hour

type uploadSession struct {
	key         string
	fileID      string
	uploadID    string
	contentHash string
	size        int64
	partSize    int64
	// nextPart 是下一个待上传分片的下标（0 基），也就是已经连续成功的分片数。
	// 只记录「连续」成功的部分：一旦第 N 个分片失败就停在 N，后面的分片即使传过也不记，
	// 因为 SectionReader 只能顺序前进，跳着读会导致缓存失配。
	nextPart  int64
	updatedAt time.Time
}

var uploadSessions = struct {
	sync.Mutex
	m map[string]*uploadSession
}{m: make(map[string]*uploadSession)}

// resumeSessionKey 用「存储 + 目标目录 + 文件名 + 大小」定位一次上传。
// 大小参与 key 是为了避免同名但内容不同的文件误续传；crypt 场景下这里是加密后的大小。
func resumeSessionKey(storageID uint, parentID, name string, size int64) string {
	return fmt.Sprintf("%d|%s|%s|%d", storageID, parentID, name, size)
}

func getUploadSession(key string, size, partSize int64) (*uploadSession, bool) {
	uploadSessions.Lock()
	defer uploadSessions.Unlock()
	s, ok := uploadSessions.m[key]
	if !ok {
		return nil, false
	}
	// 过期，或者分片参数变了（改了 CustomUploadPartSize），都不能续
	if time.Since(s.updatedAt) > uploadSessionTTL || s.size != size || s.partSize != partSize {
		delete(uploadSessions.m, key)
		return nil, false
	}
	s.updatedAt = time.Now()
	return s, true
}

func saveUploadSession(s *uploadSession) {
	if s == nil {
		return
	}
	s.updatedAt = time.Now()
	uploadSessions.Lock()
	defer uploadSessions.Unlock()
	uploadSessions.m[s.key] = s
}

func dropUploadSession(key string) {
	uploadSessions.Lock()
	defer uploadSessions.Unlock()
	delete(uploadSessions.m, key)
}

// gcExpiredUploadSessions 清掉过期会话，避免长驻进程里 map 无限增长
func gcExpiredUploadSessions() {
	uploadSessions.Lock()
	defer uploadSessions.Unlock()
	now := time.Now()
	for k, s := range uploadSessions.m {
		if now.Sub(s.updatedAt) > uploadSessionTTL {
			delete(uploadSessions.m, k)
		}
	}
}
